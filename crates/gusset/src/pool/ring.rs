//! Shared-memory completion ring (`gusset_handle_ring`).
//!
//! Without it, every completion is a `write` by the worker and a `read` by the
//! Go drain reader, and every empty poll while the reader spins is another
//! `read` that returns `EAGAIN`: two to four kernel crossings per call. With
//! it, a worker publishes the completion record into a slot of this ring and
//! the reader polls the ring with plain atomic loads. The pipe becomes a
//! doorbell: a worker writes an 8-byte wake token (ticket 0, which is never a
//! real ticket) only when the reader has announced that it is about to park.
//! In steady state a round trip then makes no system call at all.
//!
//! The ring is a bounded multi-producer, single-consumer queue in the style
//! of Vyukov's: slot `i` carries a sequence number, `i` when free for the
//! producer at position `i`, `i + 1` once that producer has published, and
//! `i + capacity` once the consumer has taken it. Capacity is the pool size
//! rounded up to a power of two. The Go side holds at most `pool_size`
//! unread completions (I4: each needs a permit, returned only after the
//! completion is read), so it never fills. If some host lets it fill anyway,
//! the record goes through the pipe as before and [`RingShared::overflow`]
//! tells the reader to look there.
//!
//! Everything shared is an atomic, so this module needs no `unsafe`: the
//! record words are written with relaxed stores and published by the
//! release store of the sequence number, which the reader loads with acquire
//! semantics before it reads them.
//!
//! The layout is part of the C ABI and is spelled out in `gusset.h`
//! (`GUSSET_RING_*`); `tests/constants_match.rs` checks the offsets.

use std::sync::atomic::{fence, AtomicU32, AtomicU64, Ordering};

use super::INLINE_RECORD_MAX;

/// Bytes per slot: the sequence word and up to [`INLINE_RECORD_MAX`] bytes
/// of record, padded to two cache lines so neighbouring slots written by
/// different workers never share a line.
pub const RING_SLOT_BYTES: usize = 128;

/// Record words per slot (the record format of the pipe protocol).
const RECORD_WORDS: usize = INLINE_RECORD_MAX / 8;

/// One slot. `words` holds a completion record exactly as the pipe protocol
/// would carry it: `[ticket | INLINE_RECORD_FLAG][len][data padded to 8]`, or a
/// bare ticket in word 0.
#[repr(C, align(128))]
pub struct Slot {
    seq: AtomicU64,
    words: [AtomicU64; RECORD_WORDS],
}

/// The ring's shared header. Each field another thread writes sits on its
/// own cache line.
#[repr(C, align(64))]
pub struct RingShared {
    /// Number of slots (a power of two). Written once, before the ring is
    /// handed out.
    pub capacity: u64,
    /// `RING_SLOT_BYTES`, so a C reader can check the stride it compiled.
    pub slot_bytes: u64,
    _pad0: [u64; 6],
    /// Set to 1 by the reader just before it parks on the pipe. The worker
    /// that next publishes swaps it back to 0 and writes one wake token.
    pub waiting: AtomicU32,
    _pad1: [u32; 15],
    /// Records that went through the pipe because the ring was full,
    /// incremented after each such write.
    pub overflow: AtomicU64,
    _pad2: [u64; 7],
    /// Next position a producer claims.
    tail: AtomicU64,
    _pad3: [u64; 7],
}

/// A completion ring: its shared header and slots.
pub struct Ring {
    shared: Box<RingShared>,
    slots: Box<[Slot]>,
    mask: u64,
}

impl Ring {
    /// A ring with room for at least `min_slots` unread completions.
    pub fn new(min_slots: usize) -> Ring {
        let capacity = min_slots.max(2).next_power_of_two();
        let slots: Box<[Slot]> = (0..capacity)
            .map(|i| Slot {
                seq: AtomicU64::new(i as u64),
                words: std::array::from_fn(|_| AtomicU64::new(0)),
            })
            .collect();
        Ring {
            shared: Box::new(RingShared {
                capacity: capacity as u64,
                slot_bytes: RING_SLOT_BYTES as u64,
                _pad0: [0; 6],
                waiting: AtomicU32::new(0),
                _pad1: [0; 15],
                overflow: AtomicU64::new(0),
                _pad2: [0; 7],
                tail: AtomicU64::new(0),
                _pad3: [0; 7],
            }),
            slots,
            mask: capacity as u64 - 1,
        }
    }

    /// The shared header, for the reader.
    pub fn shared(&self) -> &RingShared {
        &self.shared
    }

    /// The first slot, for the reader.
    pub fn slots_ptr(&self) -> *const Slot {
        self.slots.as_ptr()
    }

    /// Publishes `record` (at most `INLINE_RECORD_MAX` bytes, a multiple of
    /// 8), or returns false when the ring is full.
    pub fn try_publish(&self, record: &[u8]) -> bool {
        debug_assert!(record.len() <= INLINE_RECORD_MAX && record.len().is_multiple_of(8));
        let mut pos = self.shared.tail.load(Ordering::Relaxed);
        loop {
            let slot = &self.slots[(pos & self.mask) as usize];
            let seq = slot.seq.load(Ordering::Acquire);
            match seq.wrapping_sub(pos) as i64 {
                0 => match self.shared.tail.compare_exchange_weak(
                    pos,
                    pos.wrapping_add(1),
                    Ordering::Relaxed,
                    Ordering::Relaxed,
                ) {
                    Ok(_) => {
                        let (words, _) = record.as_chunks::<8>();
                        for (w, chunk) in slot.words.iter().zip(words) {
                            w.store(u64::from_ne_bytes(*chunk), Ordering::Relaxed);
                        }
                        slot.seq.store(pos.wrapping_add(1), Ordering::Release);
                        return true;
                    }
                    Err(now) => pos = now,
                },
                // The consumer has not freed this slot yet: full.
                d if d < 0 => return false,
                // Another producer claimed this position; catch up.
                _ => pos = self.shared.tail.load(Ordering::Relaxed),
            }
        }
    }

    /// After a publish: true if the reader announced it is parking, in which
    /// case the caller must write exactly one wake token to the pipe.
    ///
    /// The reader stores `waiting = 1` and then re-checks the ring; this side
    /// stores a slot's sequence and then loads `waiting`. The full fence makes
    /// that a Dekker pair: at least one side sees the other's store, so a
    /// completion is never left in the ring behind a parked reader.
    pub fn take_waiter(&self) -> bool {
        fence(Ordering::SeqCst);
        self.shared.waiting.load(Ordering::Relaxed) != 0
            && self.shared.waiting.swap(0, Ordering::AcqRel) != 0
    }

    /// Records that one completion went through the pipe instead.
    pub fn note_overflow(&self) {
        self.shared.overflow.fetch_add(1, Ordering::SeqCst);
    }

    /// Test helper: takes the next completion as the reader would, returning
    /// its record words.
    #[cfg(test)]
    pub fn pop_for_test(&self, head: &mut u64) -> Option<[u64; RECORD_WORDS]> {
        let slot = &self.slots[(*head & self.mask) as usize];
        if slot.seq.load(Ordering::Acquire) != head.wrapping_add(1) {
            return None;
        }
        let words = std::array::from_fn(|i| slot.words[i].load(Ordering::Relaxed));
        slot.seq
            .store(head.wrapping_add(self.mask + 1), Ordering::Release);
        *head = head.wrapping_add(1);
        Some(words)
    }
}

/// Byte offsets of the shared fields, checked against `gusset.h`.
pub const RING_OFF_CAPACITY: usize = std::mem::offset_of!(RingShared, capacity);
/// See [`RING_OFF_CAPACITY`].
pub const RING_OFF_SLOT_BYTES: usize = std::mem::offset_of!(RingShared, slot_bytes);
/// See [`RING_OFF_CAPACITY`].
pub const RING_OFF_WAITING: usize = std::mem::offset_of!(RingShared, waiting);
/// See [`RING_OFF_CAPACITY`].
pub const RING_OFF_OVERFLOW: usize = std::mem::offset_of!(RingShared, overflow);
/// Offset of the record inside a slot (the sequence word comes first).
pub const RING_SLOT_OFF_RECORD: usize = std::mem::offset_of!(Slot, words);

const _: () = assert!(std::mem::size_of::<Slot>() == RING_SLOT_BYTES);
const _: () = assert!(RING_SLOT_OFF_RECORD == 8);

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    fn rec(t: u64) -> [u8; 8] {
        t.to_ne_bytes()
    }

    #[test]
    fn capacity_rounds_up_and_full_ring_refuses() {
        let r = Ring::new(3);
        assert_eq!(r.shared().capacity, 4);
        for t in 1..=4 {
            assert!(r.try_publish(&rec(t)));
        }
        assert!(
            !r.try_publish(&rec(5)),
            "a full ring must refuse, not overwrite"
        );
        let mut head = 0;
        assert_eq!(r.pop_for_test(&mut head).map(|w| w[0]), Some(1));
        assert!(r.try_publish(&rec(5)), "a freed slot is reusable");
        let got: Vec<u64> =
            std::iter::from_fn(|| r.pop_for_test(&mut head).map(|w| w[0])).collect();
        assert_eq!(got, vec![2, 3, 4, 5]);
    }

    #[test]
    fn waiter_is_taken_exactly_once() {
        let r = Ring::new(2);
        assert!(!r.take_waiter());
        r.shared().waiting.store(1, Ordering::SeqCst);
        assert!(r.take_waiter());
        assert!(!r.take_waiter(), "one park, one token");
    }

    #[test]
    #[cfg_attr(miri, ignore)]
    fn concurrent_producers_lose_and_duplicate_nothing() {
        let r = Arc::new(Ring::new(8));
        let per = 20_000u64;
        let producers: Vec<_> = (0..4u64)
            .map(|p| {
                let r = Arc::clone(&r);
                std::thread::spawn(move || {
                    for i in 0..per {
                        let t = p * per + i + 1;
                        let mut record = [0u8; 24];
                        record[..8].copy_from_slice(&t.to_ne_bytes());
                        record[8..16].copy_from_slice(&(!t).to_ne_bytes());
                        while !r.try_publish(&record) {
                            std::hint::spin_loop();
                        }
                    }
                })
            })
            .collect();
        let mut head = 0u64;
        let mut seen = Vec::with_capacity((4 * per) as usize);
        while seen.len() < (4 * per) as usize {
            if let Some(w) = r.pop_for_test(&mut head) {
                assert_eq!(w[1], !w[0], "a record was torn");
                seen.push(w[0]);
            } else {
                std::hint::spin_loop();
            }
        }
        for p in producers {
            if p.join().is_err() {
                panic!("producer panicked");
            }
        }
        seen.sort_unstable();
        assert!(seen.iter().copied().eq(1..=4 * per));
    }
}
