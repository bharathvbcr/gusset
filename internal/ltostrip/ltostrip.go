// Package ltostrip finds and removes the LTO intermediate-representation
// sections that the go command refuses in cgo linker inputs.
//
// Go 1.26.11 and 1.27.4 (GO-2026-6853) walk every -L directory a #cgo LDFLAGS
// line or a pkg-config --libs answer names, when the directory is local to the
// build, and reject any library or object file in it that carries LTO IR: an
// ELF .llvmbc or .llvmcmd, a Mach-O __LLVM segment. A Rust staticlib always
// carries them, because it bundles the prebuilt std rlibs, which embed their
// bitcode; no rustc flag reaches those objects. The archive has to be rewritten
// without the sections before the go command sees it.
//
// The rules here are the go command's own (cmd/go/internal/work/security.go,
// isDisallowedLTOSection and the ELF and Mach-O checks), restated so that a
// check on this repository does not depend on which Go patch release ran it.
// Stripping is done member by member by the host's objcopy, and the result is
// re-read with debug/elf and debug/macho: it is written only when no member
// carries a disallowed section and every global symbol the input defined is
// still defined.
package ltostrip

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// toolTimeout bounds one objcopy run over a whole archive; ten minutes is a
// hung tool, not a slow one. probeTimeout bounds `tool --version`.
const (
	toolTimeout  = 10 * time.Minute
	probeTimeout = 30 * time.Second
)

// shtLLVMLTO is SHT_LLVM_LTO, which debug/elf has no name for.
const shtLLVMLTO = elf.SectionType(0x6fff4c0c)

// Finding is one disallowed section in one archive member.
type Finding struct {
	Member  string
	Section string // as objcopy names it: ".llvmbc", or "__LLVM,__bitcode" on Mach-O
}

func (f Finding) String() string { return f.Member + ": " + f.Section }

// disallowedName is the go command's isDisallowedLTOSection.
func disallowedName(name string) bool {
	return strings.HasPrefix(name, ".gnu.lto_") ||
		strings.HasPrefix(name, ".gnu.debuglto_") ||
		strings.HasPrefix(name, ".llvm.lto") ||
		name == ".llvmbc" ||
		name == ".llvmcmd"
}

func disallowedSegment(name string) bool { return name == "__LLVM" || name == "__GNU_LTO" }

// ObjectSections returns the disallowed sections of one object file, in the
// form objcopy takes them. An object that is neither ELF nor Mach-O is an
// error, not a clean result: a check that could not read a file must not
// report what a check that read it and found nothing reports.
func ObjectSections(data []byte) ([]string, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x7fELF")):
		f, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("unreadable ELF object: %w", err)
		}
		defer f.Close()
		var out []string
		for _, s := range f.Sections {
			if disallowedName(s.Name) || s.Type == shtLLVMLTO {
				out = append(out, s.Name)
			}
		}
		return out, nil
	case isMachO(data):
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("unreadable Mach-O object: %w", err)
		}
		defer f.Close()
		var out []string
		for _, l := range f.Loads {
			if seg, ok := l.(*macho.Segment); ok && disallowedSegment(seg.Name) && seg.Nsect == 0 {
				// An empty segment is rejected too and no section name removes it.
				return nil, fmt.Errorf("empty LTO segment %s", seg.Name)
			}
		}
		for _, s := range f.Sections {
			if disallowedSegment(s.Seg) || disallowedName(s.Name) {
				out = append(out, s.Seg+","+s.Name)
			}
		}
		return out, nil
	case bytes.HasPrefix(data, []byte("BC\xc0\xde")), bytes.HasPrefix(data, []byte{0xde, 0xc0, 0x17, 0x0b}):
		return nil, errors.New("raw LLVM bitcode member; it has no sections to remove (built with -C linker-plugin-lto?)")
	}
	return nil, errors.New("member is neither ELF nor Mach-O, so it cannot be inspected")
}

// isMachO reports a thin Mach-O object. A universal (fat) object is left out on
// purpose: debug/macho reads one slice only, and a check that read one slice
// would call the other clean.
func isMachO(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	switch [4]byte(data[:4]) {
	case [4]byte{0xcf, 0xfa, 0xed, 0xfe}, [4]byte{0xce, 0xfa, 0xed, 0xfe}, // little endian, 64 and 32 bit
		[4]byte{0xfe, 0xed, 0xfa, 0xcf}, [4]byte{0xfe, 0xed, 0xfa, 0xce}:
		return true
	}
	return false
}

// definedGlobals returns the sorted global symbols an object defines, commons
// included. Stripping sections must not change this set.
func definedGlobals(data []byte) ([]string, error) {
	var names []string
	switch {
	case bytes.HasPrefix(data, []byte("\x7fELF")):
		f, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		syms, err := f.Symbols()
		if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
			return nil, err
		}
		for _, s := range syms {
			if s.Section != elf.SHN_UNDEF && elf.ST_BIND(s.Info) != elf.STB_LOCAL {
				names = append(names, s.Name)
			}
		}
	case isMachO(data):
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if f.Symtab != nil {
			const nStab, nType, nExt = 0xe0, 0x0e, 0x01
			for _, s := range f.Symtab.Syms {
				if s.Type&nStab != 0 || s.Type&nExt == 0 {
					continue
				}
				if s.Type&nType != 0 || s.Value != 0 { // defined, or a common
					names = append(names, s.Name)
				}
			}
		}
	default:
		return nil, errors.New("member is neither ELF nor Mach-O")
	}
	sort.Strings(names)
	return names, nil
}

// Member is one archive member that is not a symbol table or the GNU long-name
// table. BSD is set when its name was a #1/len one.
type Member struct {
	Name string
	Data []byte
	BSD  bool
}

// ReadArchive returns every member of an ar archive (GNU/SysV and BSD formats).
func ReadArchive(path string) ([]Member, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseArchive(path, data)
}

func parseArchive(path string, data []byte) ([]Member, error) {
	const magic, header = "!<arch>\n", 60
	if !bytes.HasPrefix(data, []byte(magic)) {
		return nil, fmt.Errorf("%s is not an ar archive", path)
	}
	var members []Member
	var longNames []byte // the GNU "//" table, which precedes the members that name into it
	for off := len(magic); off < len(data); {
		if off+header > len(data) || string(data[off+58:off+60]) != "`\n" {
			return nil, fmt.Errorf("%s: malformed member header at offset %d", path, off)
		}
		name := strings.TrimSpace(string(data[off : off+16]))
		size, err := strconv.Atoi(strings.TrimSpace(string(data[off+48 : off+58])))
		if err != nil || size < 0 || off+header+size > len(data) {
			return nil, fmt.Errorf("%s: bad member size at offset %d", path, off)
		}
		body := data[off+header : off+header+size]
		n, bsd := strings.CutPrefix(name, "#1/")
		if bsd { // BSD: name precedes the data
			nlen, err := strconv.Atoi(n)
			if err != nil || nlen < 0 || nlen > len(body) {
				return nil, fmt.Errorf("%s: bad BSD name length at offset %d", path, off)
			}
			name, body = strings.TrimRight(string(body[:nlen]), "\x00"), body[nlen:]
		}
		switch name {
		case "//":
			longNames = body
		case "/", "/SYM64/", "__.SYMDEF", "__.SYMDEF SORTED", "__.SYMDEF_64", "__.SYMDEF_64 SORTED":
		default:
			if !bsd {
				if name, err = gnuName(name, longNames); err != nil {
					return nil, fmt.Errorf("%s: member at offset %d: %w", path, off, err)
				}
			}
			members = append(members, Member{Name: name, Data: body, BSD: bsd})
		}
		off += header + size + size%2
	}
	return members, nil
}

// gnuName returns a GNU/SysV member's real name: "/123" is an offset into the
// "//" table, whose entries end in "/\n", and a short name carries a trailing
// "/". Objcopy rebuilds that table, so the raw field of the same member differs
// between an archive and its stripped copy; the real name does not.
func gnuName(field string, table []byte) (string, error) {
	if ref, ok := strings.CutPrefix(field, "/"); ok && ref != "" {
		off, err := strconv.Atoi(ref)
		if err != nil || off < 0 || off >= len(table) {
			return "", fmt.Errorf("long name reference %q is outside the %d-byte name table", field, len(table))
		}
		end := bytes.Index(table[off:], []byte("/\n"))
		if end < 0 {
			return "", fmt.Errorf("long name at %d in the name table is unterminated", off)
		}
		return string(table[off : off+end]), nil
	}
	return strings.TrimSuffix(field, "/"), nil
}

// Scan returns every disallowed section in an archive or a single object file.
// A member it cannot read is an error.
func Scan(path string) ([]Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, []byte("!<arch>\n")) {
		secs, err := ObjectSections(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return findings(filepath.Base(path), secs), nil
	}
	members, err := parseArchive(path, data)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, m := range members {
		secs, err := ObjectSections(m.Data)
		if err != nil {
			return nil, fmt.Errorf("%s(%s): %w", path, m.Name, err)
		}
		out = append(out, findings(m.Name, secs)...)
	}
	return out, nil
}

func findings(member string, sections []string) []Finding {
	var out []Finding
	for _, s := range sections {
		out = append(out, Finding{Member: member, Section: s})
	}
	return out
}

// Result says what a Strip did.
type Result struct {
	Members  int      // members in the archive
	Stripped int      // members that carried a disallowed section
	Removed  []string // distinct section names passed to objcopy
	Tool     string   // the objcopy used; empty when nothing needed removing
}

// Strip writes out as in without its LTO IR sections. out may equal in. It
// writes out only when the stripped archive re-reads clean with every defined
// global symbol intact, and on any error leaves out as it was. An archive with
// nothing to remove is copied unchanged and needs no objcopy.
func Strip(ctx context.Context, in, out string) (Result, error) {
	data, err := os.ReadFile(in)
	if err != nil {
		return Result{}, err
	}
	members, err := parseArchive(in, data)
	if err != nil {
		return Result{}, err
	}
	res := Result{Members: len(members)}
	before := make([][]string, len(members))
	removed := map[string]bool{}
	for i, m := range members {
		secs, err := ObjectSections(m.Data)
		if err != nil {
			return res, fmt.Errorf("%s(%s): %w", in, m.Name, err)
		}
		if before[i], err = definedGlobals(m.Data); err != nil {
			return res, fmt.Errorf("%s(%s): %w", in, m.Name, err)
		}
		if len(secs) > 0 {
			res.Stripped++
		}
		for _, s := range secs {
			removed[s] = true
		}
	}
	for s := range removed {
		res.Removed = append(res.Removed, s)
	}
	sort.Strings(res.Removed)

	tmp, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*.tmp")
	if err != nil {
		return res, fmt.Errorf("cannot create a temporary file next to %s: %w", out, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return res, fmt.Errorf("cannot write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return res, fmt.Errorf("cannot close %s: %w", tmp.Name(), err)
	}

	if len(removed) > 0 {
		tool, err := FindTool(ctx)
		if err != nil {
			return res, err
		}
		res.Tool = tool
		args := make([]string, 0, len(res.Removed)+1)
		for _, s := range res.Removed {
			args = append(args, "--remove-section="+s)
		}
		args = append(args, tmp.Name())
		rctx, cancel := context.WithTimeout(ctx, toolTimeout)
		defer cancel()
		if output, err := exec.CommandContext(rctx, tool, args...).CombinedOutput(); err != nil {
			return res, fmt.Errorf("%s %s: %w\n%s", filepath.Base(tool), strings.Join(args, " "), err, output)
		}
		if err := verifyStripped(tmp.Name(), members, before); err != nil {
			return res, fmt.Errorf("%s did not produce a clean archive, %s not written: %w", filepath.Base(tool), out, err)
		}
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return res, fmt.Errorf("cannot chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return res, fmt.Errorf("cannot replace %s: %w", out, err)
	}
	return res, nil
}

// verifyStripped re-reads the archive objcopy wrote: the same members in the
// same order, none with a disallowed section, each defining the same globals.
func verifyStripped(path string, was []Member, globals [][]string) error {
	now, err := ReadArchive(path)
	if err != nil {
		return err
	}
	if len(now) != len(was) {
		return fmt.Errorf("%d members, want %d", len(now), len(was))
	}
	for i, m := range now {
		if m.Name != was[i].Name {
			return fmt.Errorf("member %d is %q, want %q", i, m.Name, was[i].Name)
		}
		secs, err := ObjectSections(m.Data)
		if err != nil {
			return fmt.Errorf("member %s: %w", m.Name, err)
		}
		if len(secs) > 0 {
			return fmt.Errorf("member %s still has %s", m.Name, strings.Join(secs, ", "))
		}
		got, err := definedGlobals(m.Data)
		if err != nil {
			return fmt.Errorf("member %s: %w", m.Name, err)
		}
		if strings.Join(got, "\x00") != strings.Join(globals[i], "\x00") {
			return fmt.Errorf("member %s changed the globals it defines (%d before, %d after)", m.Name, len(globals[i]), len(got))
		}
	}
	return nil
}

// StripTree strips, in place, every .a file under root that carries a
// disallowed section, and returns how many it rewrote. It exists for a build
// tree that was produced by a revision with no staging step (the bench job's
// base checkout), where the go command walks the whole of cargo's output.
func StripTree(ctx context.Context, root string) (int, error) {
	var rewritten int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".a") {
			return nil
		}
		res, err := Strip(ctx, path, path)
		if err != nil {
			return err
		}
		if res.Stripped > 0 {
			rewritten++
		}
		return nil
	})
	return rewritten, err
}

// FindTool returns an objcopy that accepts --remove-section on this host's
// archives and actually runs. $OBJCOPY wins. Otherwise, in order: llvm-objcopy
// on PATH, the rust-objcopy of rustc's sysroot (the llvm-tools component), and,
// off darwin, the system objcopy. A candidate is probed with --version because
// a rustup component can be present and still fail to load libLLVM.
func FindTool(ctx context.Context) (string, error) {
	if v := os.Getenv("OBJCOPY"); v != "" {
		if err := probe(ctx, v); err != nil {
			return "", fmt.Errorf("$OBJCOPY=%s does not run: %w", v, err)
		}
		return v, nil
	}
	cands := []string{"llvm-objcopy"}
	if p := rustObjcopy(ctx); p != "" {
		cands = append(cands, p)
	}
	if runtime.GOOS != "darwin" { // GNU objcopy cannot rewrite Mach-O
		cands = append(cands, "objcopy")
	}
	var tried []string
	for _, c := range cands {
		err := probe(ctx, c)
		if err == nil {
			return c, nil
		}
		tried = append(tried, fmt.Sprintf("%s (%v)", c, err))
	}
	return "", fmt.Errorf("no objcopy to strip LTO sections with; tried %s; install llvm-tools (`rustup component add llvm-tools`) or set OBJCOPY", strings.Join(tried, ", "))
}

func probe(ctx context.Context, tool string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, "--version").CombinedOutput()
	if err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if first == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, first)
	}
	return nil
}

// rustObjcopy is <sysroot>/lib/rustlib/<host>/bin/rust-objcopy, or "" when
// rustc cannot say.
func rustObjcopy(ctx context.Context) string {
	rustc := os.Getenv("RUSTC")
	if rustc == "" {
		rustc = "rustc"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	sysroot, err := exec.CommandContext(ctx, rustc, "--print", "sysroot").Output()
	if err != nil {
		return ""
	}
	verbose, err := exec.CommandContext(ctx, rustc, "-vV").Output()
	if err != nil {
		return ""
	}
	var host string
	for _, line := range strings.Split(string(verbose), "\n") {
		if h, ok := strings.CutPrefix(line, "host: "); ok {
			host = strings.TrimSpace(h)
		}
	}
	if host == "" {
		return ""
	}
	p := filepath.Join(strings.TrimSpace(string(sysroot)), "lib", "rustlib", host, "bin", "rust-objcopy")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
