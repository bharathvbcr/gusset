// Command gussetseal seals a Rust staticlib so it can share a Go binary with
// libgusset.a and with other sealed engines (R14, docs/rfc-r14.md).
//
//	gussetseal -prefix PREFIX -o OUT.a IN.a   seal IN.a into OUT.a
//	gussetseal -prefix PREFIX -verify OBJ.o   check an already-sealed object
//
// Sealing partially links every member of the archive into one relocatable
// object and makes every symbol local except the engine's own C ABI, the names
// starting with PREFIX. The engine keeps a private copy of std, its allocator
// and its panic runtime. On ELF, every COMDAT group is renamed to one only this
// engine has, because the final link keeps only the first group of each name:
// left alone, DW.ref.rust_eh_personality sends the second engine's first panic
// to a garbage address.
//
// The result is then re-read with debug/elf or debug/macho, never trusted from
// the tools' exit codes. The archive is written only when that check passes.
//
// The external tools are the host's own: CC and LIPO (defaults cc, lipo) on
// darwin, LD and OBJCOPY (defaults ld, objcopy) on Linux, and AR (default ar)
// for the output archive. On Linux, a cross target's binutils go in LD, OBJCOPY
// and AR (x86_64-linux-gnu-ld, …); on darwin the architecture comes from the
// archive.
package main

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// toolTimeout bounds each external tool. A fat-LTO archive partially links in a
// few seconds; ten minutes is a hung tool, not a slow one.
const toolTimeout = 10 * time.Minute

// maxReported caps how many offending names one error lists; the count is
// always reported in full.
const maxReported = 20

func main() {
	prefix := flag.String("prefix", "", "the engine's C ABI prefix; the only names left global (required)")
	out := flag.String("o", "", "output archive (seal mode)")
	verify := flag.Bool("verify", false, "verify an already-sealed object instead of sealing an archive")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gussetseal -prefix PREFIX -o OUT.a IN.a")
		fmt.Fprintln(os.Stderr, "       gussetseal -prefix PREFIX -verify OBJ.o")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || (*verify == (*out != "")) {
		flag.Usage()
		os.Exit(2)
	}

	var err error
	if *verify {
		err = verifyObject(flag.Arg(0), *prefix)
	} else {
		err = seal(flag.Arg(0), *prefix, *out)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetseal: %v\n", err)
		os.Exit(1)
	}
	if *verify {
		fmt.Printf("gussetseal: %s is sealed: only %s* is global\n", flag.Arg(0), *prefix)
	} else {
		fmt.Printf("gussetseal: sealed %s into %s: only %s* is global\n", flag.Arg(0), *out, *prefix)
	}
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedPrefixes are names a sealed engine may not export. gusset_ is the one
// Gusset runtime's (R14: one runtime per process), and the rest belong to Rust's
// runtime, which a seal exists to keep private.
var reservedPrefixes = []string{"gusset", "rust_", "__rust", "_R", "_ZN"}

// imageShared are runtime-protocol commons that must stay one per linked image,
// not one per engine. Every ASan-instrumented module's constructor calls
// __asan_register_elf_globals(&___asan_globals_registered, __start_asan_globals,
// __stop_asan_globals); the start/stop range is the linker's and spans every
// instrumented global in the binary, and the flag is what makes that range
// register once. Given storage per engine, each sealed engine registers the
// whole range again, Gusset's own globals included, and ASan aborts with an
// odr-violation (bench/r14 asan-rust). The seal leaves these exactly as the
// instrumentation emits them, hidden commons, which merge into one at the
// final link the way they already do across an unsealed archive's crates.
var imageShared = []string{"___asan_globals_registered"}

func isImageShared(name string) bool {
	for _, n := range imageShared {
		if name == n {
			return true
		}
	}
	return false
}

func checkPrefix(prefix string) error {
	if !identifier.MatchString(prefix) {
		return fmt.Errorf("prefix %q is not a C identifier prefix", prefix)
	}
	for _, r := range reservedPrefixes {
		if strings.HasPrefix(prefix, r) || strings.HasPrefix(r, prefix) {
			return fmt.Errorf("prefix %q overlaps the reserved %q: a sealed engine may not export Gusset's or Rust's own names", prefix, r)
		}
	}
	return nil
}

func seal(archive, prefix, out string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	if !strings.HasSuffix(out, ".a") {
		return fmt.Errorf("output %q must end in .a", out)
	}
	if _, err := os.Stat(archive); err != nil {
		return err
	}
	obj := strings.TrimSuffix(out, ".a") + ".o"

	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		list := strings.TrimSuffix(out, ".a") + ".exports"
		if err := os.WriteFile(list, []byte("_"+prefix+"*\n"), 0o644); err != nil {
			return err
		}
		arch, err := archiveArch(ctx, archive)
		if err != nil {
			return err
		}
		// The compiler driver rather than bare ld, so ld64 gets -platform_version,
		// and -arch from the archive itself: left to default, the driver assumes
		// the host's, and an x86_64 archive sealed on arm64 comes out empty.
		if err := run(ctx, env("CC", "cc"), "-arch", arch, "-r", "-nostdlib", "-o", obj,
			"-Wl,-force_load,"+archive, "-Wl,-exported_symbols_list,"+list); err != nil {
			return err
		}
	case "linux":
		if err := run(ctx, env("LD", "ld"), "-r", "--whole-archive", archive, "-o", obj); err != nil {
			return err
		}
		groups, commons, err := elfSharedNames(obj)
		if err != nil {
			return err
		}
		args := []string{"--wildcard", "--keep-global-symbol=" + prefix + "*",
			// A fat-LTO staticlib inherits .llvmbc/.llvmcmd from the prebuilt std
			// rlibs. ld -r concatenates them into one invalid bitcode blob, which
			// binutils' ar aborts on when the LLVMgold plugin is installed.
			"--remove-section=.llvmbc", "--remove-section=.llvmcmd"}
		for _, g := range groups {
			if !engineOwned(g, prefix) {
				args = append(args, "--redefine-sym", g+"="+g+"."+prefix)
			}
		}
		// A common has no section, so objcopy cannot localize it, and the final
		// link merges same-named commons across engines. Each one is renamed to
		// a name only this engine has, and kept global so it still has storage;
		// the image-wide protocol commons are left to merge on purpose.
		for _, c := range commons {
			switch {
			case isImageShared(c):
				args = append(args, "--keep-global-symbol="+c)
			case !engineOwned(c, prefix):
				args = append(args, "--redefine-sym", c+"="+c+"."+prefix, "--keep-global-symbol="+c+"."+prefix)
			}
		}
		if err := run(ctx, env("OBJCOPY", "objcopy"), append(args, obj)...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("no sealing recipe for %s; R14 allows only the single unsealed archive there", runtime.GOOS)
	}

	if err := verifyObject(obj, prefix); err != nil {
		return fmt.Errorf("sealed object failed verification, %s not written: %w", out, err)
	}
	if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := run(ctx, env("AR", "ar"), "rcs", out, obj); err != nil {
		return err
	}
	// Verify what ships, not only what was verified: the archive must hold
	// exactly the object that passed, byte for byte.
	if err := archiveHoldsExactly(out, obj); err != nil {
		os.Remove(out)
		return fmt.Errorf("%s does not hold the verified object, removed: %w", out, err)
	}
	return nil
}

// archiveHoldsExactly fails unless archive's only member, symbol tables aside,
// has the same bytes as obj. It reads both the GNU/SysV format (ar on Linux)
// and the BSD one with #1/len names (ar on darwin).
func archiveHoldsExactly(archive, obj string) error {
	want, err := os.ReadFile(obj)
	if err != nil {
		return err
	}
	members, err := archiveMembers(archive)
	if err != nil {
		return err
	}
	if len(members) != 1 {
		return fmt.Errorf("%d members, want 1", len(members))
	}
	if !bytes.Equal(members[0], want) {
		return fmt.Errorf("its member (%d bytes) differs from %s (%d bytes)", len(members[0]), obj, len(want))
	}
	return nil
}

// archiveMembers returns the contents of every member that is not a symbol
// table or the GNU long-name table.
func archiveMembers(archive string) ([][]byte, error) {
	data, err := os.ReadFile(archive)
	if err != nil {
		return nil, err
	}
	const magic, header = "!<arch>\n", 60
	if !bytes.HasPrefix(data, []byte(magic)) {
		return nil, fmt.Errorf("%s is not an ar archive", archive)
	}
	var members [][]byte
	for off := len(magic); off < len(data); {
		if off+header > len(data) || string(data[off+58:off+60]) != "`\n" {
			return nil, fmt.Errorf("%s: malformed member header at offset %d", archive, off)
		}
		name := strings.TrimSpace(string(data[off : off+16]))
		size, err := strconv.Atoi(strings.TrimSpace(string(data[off+48 : off+58])))
		if err != nil || size < 0 || off+header+size > len(data) {
			return nil, fmt.Errorf("%s: bad member size at offset %d", archive, off)
		}
		body := data[off+header : off+header+size]
		if n, ok := strings.CutPrefix(name, "#1/"); ok { // BSD: name precedes the data
			nlen, err := strconv.Atoi(n)
			if err != nil || nlen > len(body) {
				return nil, fmt.Errorf("%s: bad BSD name length at offset %d", archive, off)
			}
			name, body = strings.TrimRight(string(body[:nlen]), "\x00"), body[nlen:]
		}
		switch name {
		case "/", "//", "/SYM64/", "__.SYMDEF", "__.SYMDEF SORTED", "__.SYMDEF_64", "__.SYMDEF_64 SORTED":
		default:
			members = append(members, body)
		}
		off += header + size + size%2
	}
	return members, nil
}

// archiveArch returns the one architecture a Mach-O archive holds. A universal
// archive is refused: each slice needs its own seal (lipo -thin, seal, lipo
// -create), and sealing only the host's slice would silently drop the rest.
func archiveArch(ctx context.Context, archive string) (string, error) {
	cmd := exec.CommandContext(ctx, env("LIPO", "lipo"), "-archs", archive)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("lipo -archs %s: %w", archive, err)
	}
	archs := strings.Fields(string(output))
	if len(archs) != 1 {
		return "", fmt.Errorf("%s holds %d architectures %v; seal each slice separately (lipo -thin) and recombine with lipo -create", archive, len(archs), archs)
	}
	return archs[0], nil
}

// engineOwned reports whether a COMDAT signature is already unique to this
// engine: one of its exports, or a name this tool renamed for it.
func engineOwned(sig, prefix string) bool {
	return strings.HasPrefix(sig, prefix) || strings.HasSuffix(sig, "."+prefix)
}

// verifyObject fails unless obj exports at least one PREFIX* name and nothing
// else, and (ELF) every COMDAT group it carries is named for this engine alone.
func verifyObject(obj, prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	var exported, leaked, shared []string
	if f, err := elf.Open(obj); err == nil {
		defer f.Close()
		syms, err := f.Symbols()
		if err != nil {
			return fmt.Errorf("%s: reading symbols: %w", obj, err)
		}
		// STB_GNU_UNIQUE is 10; debug/elf has no name for it.
		const stbGNUUnique = elf.SymBind(10)
		for _, s := range syms {
			b := elf.ST_BIND(s.Info)
			if s.Section == elf.SHN_UNDEF || (b != elf.STB_GLOBAL && b != elf.STB_WEAK && b != stbGNUUnique) {
				continue
			}
			switch {
			case strings.HasPrefix(s.Name, prefix):
				exported = append(exported, s.Name)
			case s.Section == elf.SHN_COMMON && (isImageShared(s.Name) || engineOwned(s.Name, prefix)):
				// A protocol common meant to merge image-wide, or a common renamed
				// for this engine alone.
			default:
				leaked = append(leaked, s.Name)
			}
		}
		groups, err := groupSignatures(f)
		if err != nil {
			return fmt.Errorf("%s: %w", obj, err)
		}
		for _, g := range groups {
			if !engineOwned(g, prefix) {
				shared = append(shared, g)
			}
		}
	} else if f, merr := macho.Open(obj); merr == nil {
		defer f.Close()
		if f.Symtab == nil {
			return fmt.Errorf("%s: no symbol table", obj)
		}
		for _, s := range f.Symtab.Syms {
			// A private external (N_EXT|N_PEXT) still binds across object files
			// in the final link, so it counts as global here. ld -r turns hidden
			// symbols into statics, but leaves a common symbol private external,
			// and ld64 has no -d to give it storage: such an archive is refused.
			const nStab, nType, nExt, nUndf = 0xe0, 0x0e, 0x01, 0x00
			if s.Type&nStab != 0 || s.Type&nExt == 0 {
				continue
			}
			if s.Type&nType == nUndf && s.Value == 0 { // undefined, not common
				continue
			}
			name := strings.TrimPrefix(s.Name, "_")
			if strings.HasPrefix(name, prefix) {
				exported = append(exported, name)
			} else {
				leaked = append(leaked, name)
			}
		}
	} else {
		return fmt.Errorf("%s is neither ELF (%v) nor Mach-O (%v)", obj, err, merr)
	}

	var problems []string
	if len(leaked) > 0 {
		problems = append(problems, fmt.Sprintf("%d global(s) outside %s*: %s", len(leaked), prefix, capped(leaked)))
	}
	if len(shared) > 0 {
		problems = append(problems, fmt.Sprintf("%d COMDAT group(s) another engine can share, so the final link may discard this engine's copy: %s", len(shared), capped(shared)))
	}
	if len(exported) == 0 {
		problems = append(problems, fmt.Sprintf("no global named %s*: the engine would export nothing", prefix))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s is not sealed: %s", obj, strings.Join(problems, "; "))
	}
	return nil
}

// elfSharedNames returns the names in obj the final link could merge with
// another object's: COMDAT group signatures and global common symbols.
func elfSharedNames(obj string) (groups, commons []string, err error) {
	f, err := elf.Open(obj)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if groups, err = groupSignatures(f); err != nil {
		return nil, nil, err
	}
	syms, err := f.Symbols()
	if err != nil {
		return nil, nil, fmt.Errorf("reading symbols for commons: %w", err)
	}
	for _, s := range syms {
		if s.Section == elf.SHN_COMMON && elf.ST_BIND(s.Info) != elf.STB_LOCAL {
			commons = append(commons, s.Name)
		}
	}
	return groups, commons, nil
}

// groupSignatures returns the signature symbol of every SHT_GROUP section. The
// group's sh_link names the symbol table and sh_info the symbol's index in it;
// debug/elf's Symbols omits index 0, hence the -1.
func groupSignatures(f *elf.File) ([]string, error) {
	var sigs []string
	var syms []elf.Symbol
	for _, sec := range f.Sections {
		if sec.Type != elf.SHT_GROUP {
			continue
		}
		if syms == nil {
			var err error
			if syms, err = f.Symbols(); err != nil {
				return nil, fmt.Errorf("reading symbols for COMDAT groups: %w", err)
			}
		}
		i := int(sec.Info) - 1
		if i < 0 || i >= len(syms) {
			return nil, fmt.Errorf("group %s names symbol %d of %d", sec.Name, sec.Info, len(syms))
		}
		sigs = append(sigs, syms[i].Name)
	}
	return sigs, nil
}

func capped(names []string) string {
	sort.Strings(names)
	if len(names) > maxReported {
		return strings.Join(names[:maxReported], ", ") + fmt.Sprintf(", … (%d more)", len(names)-maxReported)
	}
	return strings.Join(names, ", ")
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func run(ctx context.Context, tool string, args ...string) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", filepath.Base(tool), strings.Join(args, " "), err, output)
	}
	return nil
}
