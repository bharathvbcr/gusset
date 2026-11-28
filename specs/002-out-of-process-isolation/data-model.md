# Data Model

## 1. Entities & Types

| Entity | Defined in | Notes |
| :--- | :--- | :--- |
| `request` | `internal/isolate/wire.go` | `op` (`hello`, `submit`, `cancel`), ticket `id`, `in` bytes, relative `timeout_ns`, optional `opcode`, optional 24-byte trace carrier. |
| `response` | `internal/isolate/wire.go` | `op`, `id`, `out`, `err`, and for the hello frame `version` and `pool_size`. |
| `wireError` | `internal/isolate/wire.go` | A `*gusset.Error` crosses as code, message, file and line so `errors.Is` works by code; other errors cross as a kind (`closed`, `shutdown`, `deadline`, `canceled`, `other`) plus text. |
| `Proc` | `internal/isolate/proc.go` | Host side of one worker. Methods `Call`, `Submit`, `Wait`, `Discard`, `Close`; poison-on-death. |
| `Supervisor`, `Policy` | `internal/isolate/supervisor.go` | `Policy` carries `MaxRestarts`, `Window`, `StartTimeout`, `StallTimeout`. `DefaultPolicy` is 3 restarts in one minute. |
| `Caller` | `internal/isolate/` | The five-method interface shared by the in-process handle and `*Proc`, used by the parity tests. |

## 2. Invariants

- **Protocol version** is 1 and is checked in the worker's hello frame (`protocolVersion`).
- **Frame bounds**: a request frame is at most `64 KiB`; a response frame is at most `gusset.MaxBufferBytes/3*4 + 64 KiB`, because base64 inflates a result by 4/3.
- **Inline input limit** is 4096 bytes, matching the in-process copy limit (R16), so one mode never accepts what the other refuses.
- **Time** crosses as a relative `timeout_ns` (0 none, 1 already expired); no absolute clock reading crosses (R9).
- **Ticket space**: ids are not wrapped onto a live id (`errTicketSpace`).
- **Poison**: a retired generation fails its in-flight tickets with an error matching `gusset.ErrPoisoned`; a result stored before death is still returned by `Wait`.

## 3. Serialisation

Frame: 4-byte big-endian length, then a JSON body. Byte fields are base64 under `encoding/json`. The stand-in is internal and may change without a public contract change.
