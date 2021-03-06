// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package csource generates [almost] equivalent C programs from syzkaller programs.
package csource

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unsafe"

	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys/targets"
)

type Options struct {
	Threaded bool
	Collide  bool
	Repeat   bool
	Procs    int
	Sandbox  string

	Fault     bool // inject fault into FaultCall/FaultNth
	FaultCall int
	FaultNth  int

	// These options allow for a more fine-tuned control over the generated C code.
	EnableTun  bool
	UseTmpDir  bool
	HandleSegv bool
	WaitRepeat bool
	Debug      bool

	// Generate code for use with repro package to prints log messages,
	// which allows to distinguish between a hang and an absent crash.
	Repro bool
}

// Check checks if the opts combination is valid or not.
// For example, Collide without Threaded is not valid.
// Invalid combinations must not be passed to Write.
func (opts Options) Check() error {
	if !opts.Threaded && opts.Collide {
		// Collide requires threaded.
		return errors.New("Collide without Threaded")
	}
	if !opts.Repeat && opts.Procs > 1 {
		// This does not affect generated code.
		return errors.New("Procs>1 without Repeat")
	}
	if opts.Sandbox == "namespace" && !opts.UseTmpDir {
		// This is borken and never worked.
		// This tries to create syz-tmp dir in cwd,
		// which will fail if procs>1 and on second run of the program.
		return errors.New("Sandbox=namespace without UseTmpDir")
	}
	return nil
}

func Write(p *prog.Prog, opts Options) ([]byte, error) {
	if err := opts.Check(); err != nil {
		return nil, fmt.Errorf("csource: invalid opts: %v", err)
	}
	commonHeader := ""
	switch p.Target.OS {
	case "linux":
		commonHeader = commonHeaderLinux
	case "akaros":
		commonHeader = commonHeaderAkaros
	default:
		return nil, fmt.Errorf("unsupported OS: %v", p.Target.OS)
	}
	ctx := &context{
		p:         p,
		opts:      opts,
		target:    p.Target,
		sysTarget: targets.List[p.Target.OS][p.Target.Arch],
		w:         new(bytes.Buffer),
		calls:     make(map[string]uint64),
	}
	for _, c := range p.Calls {
		ctx.calls[c.Meta.CallName] = c.Meta.NR
	}

	ctx.print("// autogenerated by syzkaller (http://github.com/google/syzkaller)\n\n")

	hdr, err := ctx.preprocessCommonHeader(commonHeader)
	if err != nil {
		return nil, err
	}
	ctx.print(hdr)
	ctx.print("\n")

	ctx.generateSyscallDefines()

	exec := make([]byte, prog.ExecBufferSize)
	progSize, err := ctx.p.SerializeForExec(exec, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize program: %v", err)
	}
	calls, nvar := ctx.generateCalls(exec[:progSize])
	ctx.printf("long r[%v];\n", nvar)

	if !opts.Repeat {
		ctx.generateTestFunc(calls, "loop")

		ctx.print("int main()\n{\n")
		if opts.HandleSegv {
			ctx.printf("\tinstall_segv_handler();\n")
		}
		if opts.UseTmpDir {
			ctx.printf("\tuse_temporary_dir();\n")
		}
		if opts.Sandbox != "" {
			ctx.printf("\tint pid = do_sandbox_%v(0, %v);\n", opts.Sandbox, opts.EnableTun)
			ctx.print("\tint status = 0;\n")
			ctx.print("\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
		} else {
			if opts.EnableTun {
				ctx.printf("\tsetup_tun(0, %v);\n", opts.EnableTun)
			}
			ctx.print("\tloop();\n")
		}
		ctx.print("\treturn 0;\n}\n")
	} else {
		ctx.generateTestFunc(calls, "test")
		if opts.Procs <= 1 {
			ctx.print("int main()\n{\n")
			if opts.HandleSegv {
				ctx.printf("\tinstall_segv_handler();\n")
			}
			if opts.UseTmpDir {
				ctx.printf("\tuse_temporary_dir();\n")
			}
			if opts.Sandbox != "" {
				ctx.printf("\tint pid = do_sandbox_%v(0, %v);\n", opts.Sandbox, opts.EnableTun)
				ctx.print("\tint status = 0;\n")
				ctx.print("\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
			} else {
				if opts.EnableTun {
					ctx.printf("\tsetup_tun(0, %v);\n", opts.EnableTun)
				}
				ctx.print("\tloop();\n")
			}
			ctx.print("\treturn 0;\n}\n")
		} else {
			ctx.print("int main()\n{\n")
			ctx.print("\tint i;")
			ctx.printf("\tfor (i = 0; i < %v; i++) {\n", opts.Procs)
			ctx.print("\t\tif (fork() == 0) {\n")
			if opts.HandleSegv {
				ctx.printf("\t\t\tinstall_segv_handler();\n")
			}
			if opts.UseTmpDir {
				ctx.printf("\t\t\tuse_temporary_dir();\n")
			}
			if opts.Sandbox != "" {
				ctx.printf("\t\t\tint pid = do_sandbox_%v(i, %v);\n", opts.Sandbox, opts.EnableTun)
				ctx.print("\t\t\tint status = 0;\n")
				ctx.print("\t\t\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
			} else {
				if opts.EnableTun {
					ctx.printf("\t\t\tsetup_tun(i, %v);\n", opts.EnableTun)
				}
				ctx.print("\t\t\tloop();\n")
			}
			ctx.print("\t\t\treturn 0;\n")
			ctx.print("\t\t}\n")
			ctx.print("\t}\n")
			ctx.print("\tsleep(1000000);\n")
			ctx.print("\treturn 0;\n}\n")
		}
	}

	// Remove NONFAILING and debug calls.
	out0 := ctx.w.String()
	if !opts.HandleSegv {
		re := regexp.MustCompile(`\t*NONFAILING\((.*)\);\n`)
		out0 = re.ReplaceAllString(out0, "$1;\n")
	}
	if !opts.Debug {
		re := regexp.MustCompile(`\t*debug\(.*\);\n`)
		out0 = re.ReplaceAllString(out0, "")
		re = regexp.MustCompile(`\t*debug_dump_data\(.*\);\n`)
		out0 = re.ReplaceAllString(out0, "")
	}
	out0 = strings.Replace(out0, "NORETURN", "", -1)

	// Remove duplicate new lines.
	out1 := []byte(out0)
	for {
		out2 := bytes.Replace(out1, []byte{'\n', '\n', '\n'}, []byte{'\n', '\n'}, -1)
		if len(out1) == len(out2) {
			break
		}
		out1 = out2
	}

	return out1, nil
}

type context struct {
	p         *prog.Prog
	opts      Options
	target    *prog.Target
	sysTarget *targets.Target
	w         *bytes.Buffer
	calls     map[string]uint64 // CallName -> NR
}

func (ctx *context) print(str string) {
	ctx.w.WriteString(str)
}

func (ctx *context) printf(str string, args ...interface{}) {
	ctx.print(fmt.Sprintf(str, args...))
}

func (ctx *context) generateTestFunc(calls []string, name string) {
	opts := ctx.opts
	if !opts.Threaded && !opts.Collide {
		ctx.printf("void %v()\n{\n", name)
		if opts.Debug {
			// Use debug to avoid: error: ‘debug’ defined but not used.
			ctx.printf("\tdebug(\"%v\\n\");\n", name)
		}
		if opts.Repro {
			ctx.printf("\tsyscall(SYS_write, 1, \"executing program\\n\", strlen(\"executing program\\n\"));\n")
		}
		ctx.printf("\tmemset(r, -1, sizeof(r));\n")
		for _, c := range calls {
			ctx.printf("%s", c)
		}
		ctx.printf("}\n\n")
	} else {
		ctx.printf("void *thr(void *arg)\n{\n")
		ctx.printf("\tswitch ((long)arg) {\n")
		for i, c := range calls {
			ctx.printf("\tcase %v:\n", i)
			ctx.printf("%s", strings.Replace(c, "\t", "\t\t", -1))
			ctx.printf("\t\tbreak;\n")
		}
		ctx.printf("\t}\n")
		ctx.printf("\treturn 0;\n}\n\n")

		ctx.printf("void %v()\n{\n", name)
		ctx.printf("\tlong i;\n")
		ctx.printf("\tpthread_t th[%v];\n", 2*len(calls))
		ctx.printf("\n")
		if opts.Debug {
			// Use debug to avoid: error: ‘debug’ defined but not used.
			ctx.printf("\tdebug(\"%v\\n\");\n", name)
		}
		if opts.Repro {
			ctx.printf("\tsyscall(SYS_write, 1, \"executing program\\n\", strlen(\"executing program\\n\"));\n")
		}
		ctx.printf("\tmemset(r, -1, sizeof(r));\n")
		if opts.Collide {
			ctx.printf("\tsrand(getpid());\n")
		}
		ctx.printf("\tfor (i = 0; i < %v; i++) {\n", len(calls))
		ctx.printf("\t\tpthread_create(&th[i], 0, thr, (void*)i);\n")
		ctx.printf("\t\tusleep(rand()%%10000);\n")
		ctx.printf("\t}\n")
		if opts.Collide {
			ctx.printf("\tfor (i = 0; i < %v; i++) {\n", len(calls))
			ctx.printf("\t\tpthread_create(&th[%v+i], 0, thr, (void*)i);\n", len(calls))
			ctx.printf("\t\tif (rand()%%2)\n")
			ctx.printf("\t\t\tusleep(rand()%%10000);\n")
			ctx.printf("\t}\n")
		}
		ctx.printf("\tusleep(rand()%%100000);\n")
		ctx.printf("}\n\n")
	}
}

func (ctx *context) generateSyscallDefines() {
	prefix := ctx.sysTarget.SyscallPrefix
	for name, nr := range ctx.calls {
		if strings.HasPrefix(name, "syz_") || !ctx.sysTarget.NeedSyscallDefine(nr) {
			continue
		}
		ctx.printf("#ifndef %v%v\n", prefix, name)
		ctx.printf("#define %v%v %v\n", prefix, name, nr)
		ctx.printf("#endif\n")
	}
	if ctx.target.OS == "linux" && ctx.target.PtrSize == 4 {
		// This is a dirty hack.
		// On 32-bit linux mmap translated to old_mmap syscall which has a different signature.
		// mmap2 has the right signature. executor translates mmap to mmap2, do the same here.
		ctx.printf("#undef __NR_mmap\n")
		ctx.printf("#define __NR_mmap __NR_mmap2\n")
	}
	ctx.printf("\n")
}

func (ctx *context) generateCalls(exec []byte) ([]string, int) {
	read := func() uint64 {
		if len(exec) < 8 {
			panic("exec program overflow")
		}
		v := *(*uint64)(unsafe.Pointer(&exec[0]))
		exec = exec[8:]
		return v
	}
	resultRef := func() string {
		arg := read()
		res := fmt.Sprintf("r[%v]", arg)
		if opDiv := read(); opDiv != 0 {
			res = fmt.Sprintf("%v/%v", res, opDiv)
		}
		if opAdd := read(); opAdd != 0 {
			res = fmt.Sprintf("%v+%v", res, opAdd)
		}
		return res
	}
	lastCall := 0
	seenCall := false
	var calls []string
	w := new(bytes.Buffer)
	newCall := func() {
		if seenCall {
			seenCall = false
			calls = append(calls, w.String())
			w = new(bytes.Buffer)
		}
	}
	n := 0
loop:
	for ; ; n++ {
		switch instr := read(); instr {
		case prog.ExecInstrEOF:
			break loop
		case prog.ExecInstrCopyin:
			newCall()
			addr := read()
			typ := read()
			size := read()
			switch typ {
			case prog.ExecArgConst:
				arg := read()
				bfOff := read()
				bfLen := read()
				if bfOff == 0 && bfLen == 0 {
					fmt.Fprintf(w, "\tNONFAILING(*(uint%v_t*)0x%x = (uint%v_t)0x%x);\n", size*8, addr, size*8, arg)
				} else {
					fmt.Fprintf(w, "\tNONFAILING(STORE_BY_BITMASK(uint%v_t, 0x%x, 0x%x, %v, %v));\n", size*8, addr, arg, bfOff, bfLen)
				}
			case prog.ExecArgResult:
				fmt.Fprintf(w, "\tNONFAILING(*(uint%v_t*)0x%x = %v);\n", size*8, addr, resultRef())
			case prog.ExecArgData:
				data := exec[:size]
				exec = exec[(size+7)/8*8:]
				var esc []byte
				for _, v := range data {
					hex := func(v byte) byte {
						if v < 10 {
							return '0' + v
						}
						return 'a' + v - 10
					}
					esc = append(esc, '\\', 'x', hex(v>>4), hex(v<<4>>4))
				}
				fmt.Fprintf(w, "\tNONFAILING(memcpy((void*)0x%x, \"%s\", %v));\n", addr, esc, size)
			case prog.ExecArgCsum:
				csum_kind := read()
				switch csum_kind {
				case prog.ExecArgCsumInet:
					fmt.Fprintf(w, "\tstruct csum_inet csum_%d;\n", n)
					fmt.Fprintf(w, "\tcsum_inet_init(&csum_%d);\n", n)
					csumChunksNum := read()
					for i := uint64(0); i < csumChunksNum; i++ {
						chunk_kind := read()
						chunk_value := read()
						chunk_size := read()
						switch chunk_kind {
						case prog.ExecArgCsumChunkData:
							fmt.Fprintf(w, "\tNONFAILING(csum_inet_update(&csum_%d, (const uint8_t*)0x%x, %d));\n", n, chunk_value, chunk_size)
						case prog.ExecArgCsumChunkConst:
							fmt.Fprintf(w, "\tuint%d_t csum_%d_chunk_%d = 0x%x;\n", chunk_size*8, n, i, chunk_value)
							fmt.Fprintf(w, "\tcsum_inet_update(&csum_%d, (const uint8_t*)&csum_%d_chunk_%d, %d);\n", n, n, i, chunk_size)
						default:
							panic(fmt.Sprintf("unknown checksum chunk kind %v", chunk_kind))
						}
					}
					fmt.Fprintf(w, "\tNONFAILING(*(uint16_t*)0x%x = csum_inet_digest(&csum_%d));\n", addr, n)
				default:
					panic(fmt.Sprintf("unknown csum kind %v", csum_kind))
				}
			default:
				panic(fmt.Sprintf("bad argument type %v", instr))
			}
		case prog.ExecInstrCopyout:
			addr := read()
			size := read()
			fmt.Fprintf(w, "\tif (r[%v] != -1)\n", lastCall)
			fmt.Fprintf(w, "\t\tNONFAILING(r[%v] = *(uint%v_t*)0x%x);\n", n, size*8, addr)
		default:
			// Normal syscall.
			newCall()
			if ctx.opts.Fault && ctx.opts.FaultCall == len(calls) {
				fmt.Fprintf(w, "\twrite_file(\"/sys/kernel/debug/failslab/ignore-gfp-wait\", \"N\");\n")
				fmt.Fprintf(w, "\twrite_file(\"/sys/kernel/debug/fail_futex/ignore-private\", \"N\");\n")
				fmt.Fprintf(w, "\tinject_fault(%v);\n", ctx.opts.FaultNth)
			}
			meta := ctx.target.Syscalls[instr]
			emitCall := true
			if meta.CallName == "syz_test" {
				emitCall = false
			}
			if !ctx.opts.EnableTun && (meta.CallName == "syz_emit_ethernet" ||
				meta.CallName == "syz_extract_tcp_res") {
				emitCall = false
			}
			native := !strings.HasPrefix(meta.CallName, "syz_")
			if emitCall {
				if native {
					fmt.Fprintf(w, "\tr[%v] = syscall(%v%v",
						n, ctx.sysTarget.SyscallPrefix, meta.CallName)
				} else {
					fmt.Fprintf(w, "\tr[%v] = %v(", n, meta.CallName)
				}
			}
			nargs := read()
			for i := uint64(0); i < nargs; i++ {
				typ := read()
				size := read()
				_ = size
				if emitCall && (native || i > 0) {
					fmt.Fprintf(w, ", ")
				}
				switch typ {
				case prog.ExecArgConst:
					value := read()
					if emitCall {
						fmt.Fprintf(w, "0x%xul", value)
					}
					// Bitfields can't be args of a normal syscall, so just ignore them.
					read() // bit field offset
					read() // bit field length
				case prog.ExecArgResult:
					ref := resultRef()
					if emitCall {
						fmt.Fprintf(w, "%v", ref)
					}
				default:
					panic(fmt.Sprintf("unknown arg type %v", typ))
				}
			}
			if emitCall {
				fmt.Fprintf(w, ");\n")
			}
			lastCall = n
			seenCall = true
		}
	}
	newCall()
	return calls, n
}

func (ctx *context) preprocessCommonHeader(commonHeader string) (string, error) {
	var defines []string
	if prog.RequiresBitmasks(ctx.p) {
		defines = append(defines, "SYZ_USE_BITMASKS")
	}
	if prog.RequiresChecksums(ctx.p) {
		defines = append(defines, "SYZ_USE_CHECKSUMS")
	}
	opts := ctx.opts
	switch opts.Sandbox {
	case "":
		// No sandbox, do nothing.
	case "none":
		defines = append(defines, "SYZ_SANDBOX_NONE")
	case "setuid":
		defines = append(defines, "SYZ_SANDBOX_SETUID")
	case "namespace":
		defines = append(defines, "SYZ_SANDBOX_NAMESPACE")
	default:
		return "", fmt.Errorf("unknown sandbox mode: %v", opts.Sandbox)
	}
	if opts.Threaded {
		defines = append(defines, "SYZ_THREADED")
	}
	if opts.Collide {
		defines = append(defines, "SYZ_COLLIDE")
	}
	if opts.Repeat {
		defines = append(defines, "SYZ_REPEAT")
	}
	if opts.Fault {
		defines = append(defines, "SYZ_FAULT_INJECTION")
	}
	if opts.EnableTun {
		defines = append(defines, "SYZ_TUN_ENABLE")
	}
	if opts.UseTmpDir {
		defines = append(defines, "SYZ_USE_TMP_DIR")
	}
	if opts.HandleSegv {
		defines = append(defines, "SYZ_HANDLE_SEGV")
	}
	if opts.WaitRepeat {
		defines = append(defines, "SYZ_WAIT_REPEAT")
	}
	if opts.Debug {
		defines = append(defines, "SYZ_DEBUG")
	}
	for name, _ := range ctx.calls {
		defines = append(defines, "__NR_"+name)
	}
	defines = append(defines, ctx.sysTarget.CArch...)

	cmd := exec.Command("cpp", "-nostdinc", "-undef", "-fdirectives-only", "-dDI", "-E", "-P", "-")
	for _, def := range defines {
		cmd.Args = append(cmd.Args, "-D"+def)
	}
	cmd.Stdin = strings.NewReader(commonHeader)
	stderr := new(bytes.Buffer)
	stdout := new(bytes.Buffer)
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	if err := cmd.Run(); len(stdout.Bytes()) == 0 {
		return "", fmt.Errorf("cpp failed: %v\n%v\n%v\n", err, stdout.String(), stderr.String())
	}
	remove := append(defines, []string{
		"__STDC__",
		"__STDC_HOSTED__",
		"__STDC_UTF_16__",
		"__STDC_UTF_32__",
	}...)
	out := stdout.String()
	for _, def := range remove {
		out = strings.Replace(out, "#define "+def+" 1\n", "", -1)
	}
	// strip: #define __STDC_VERSION__ 201112L
	for _, def := range []string{"__STDC_VERSION__"} {
		pos := strings.Index(out, "#define "+def)
		if pos == -1 {
			continue
		}
		end := strings.IndexByte(out[pos:], '\n')
		if end == -1 {
			continue
		}
		out = strings.Replace(out, out[pos:end+1], "", -1)
	}
	return out, nil
}

// Build builds a C/C++ program from source src and returns name of the resulting binary.
// lang can be "c" or "c++".
func Build(target *prog.Target, lang, src string) (string, error) {
	bin, err := ioutil.TempFile("", "syzkaller")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	bin.Close()
	sysTarget := targets.List[target.OS][target.Arch]
	compiler := sysTarget.CCompilerPrefix + "gcc"
	if _, err := exec.LookPath(compiler); err != nil {
		return "", NoCompilerErr
	}
	flags := []string{
		"-x", lang, "-Wall", "-Werror", "-O1", "-g", "-o", bin.Name(),
		src, "-pthread",
	}
	flags = append(flags, sysTarget.CrossCFlags...)
	if sysTarget.PtrSize == 4 {
		// We do generate uint64's for syscall arguments that overflow longs on 32-bit archs.
		flags = append(flags, "-Wno-overflow")
	}
	out, err := exec.Command(compiler, append(flags, "-static")...).CombinedOutput()
	if err != nil {
		// Some distributions don't have static libraries.
		out, err = exec.Command(compiler, flags...).CombinedOutput()
	}
	if err != nil {
		os.Remove(bin.Name())
		data, _ := ioutil.ReadFile(src)
		return "", fmt.Errorf("failed to build program:\n%s\n%s\ncompiler invocation: %v %v\n",
			data, out, compiler, flags)
	}
	return bin.Name(), nil
}

var NoCompilerErr = errors.New("no target compiler")

// Format reformats C source using clang-format.
func Format(src []byte) ([]byte, error) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd := exec.Command("clang-format", "-assume-filename=/src.c", "-style", style)
	cmd.Stdin = bytes.NewReader(src)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return src, fmt.Errorf("failed to format source: %v\n%v", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Something acceptable for kernel developers and email-friendly.
var style = `{
BasedOnStyle: LLVM,
IndentWidth: 2,
UseTab: Never,
BreakBeforeBraces: Linux,
IndentCaseLabels: false,
DerivePointerAlignment: false,
PointerAlignment: Left,
AlignTrailingComments: true,
AllowShortBlocksOnASingleLine: false,
AllowShortCaseLabelsOnASingleLine: false,
AllowShortFunctionsOnASingleLine: false,
AllowShortIfStatementsOnASingleLine: false,
AllowShortLoopsOnASingleLine: false,
ColumnLimit: 72,
}`
