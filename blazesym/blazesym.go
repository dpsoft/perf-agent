package blazesym

/*

#cgo LDFLAGS: -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static

#include "blazesym.h"
// HACK
// The generated struct in cgo does not contain syms for blazesym result
// see:
//type _Ctype_struct_blaze_syms struct {
//	cnt _Ctype_size_t
//}
// Adding a C function to return syms from blaze_result
struct blaze_sym* get_result(blaze_syms* res, size_t pos) {
	return &res->syms[pos];
}
*/
import "C"
import (
	"errors"
	"fmt"
	"unsafe"
)

const unknownSymbol string = "[Unknown]"

// Symbolizer represents a Blazesym symbolizer.
type Symbolizer struct {
	s *C.blaze_symbolizer
}

type BlazeErr int

const (
	blazeErrOk               BlazeErr = 0
	blazeErrNotFound         BlazeErr = -2
	blazeErrPermissionDenied BlazeErr = -1
	blazeErrAlreadyExists    BlazeErr = -17
	blazeErrWouldBlock       BlazeErr = -11
	blazeErrInvalidData      BlazeErr = -22
	blazeErrTimedOut         BlazeErr = -110
	blazeErrUnsupported      BlazeErr = -95
	blazeErrOutOfMemory      BlazeErr = -12
	blazeErrInvalidInput     BlazeErr = -256
	blazeErrWriteZero        BlazeErr = -257
	blazeErrUnexpectedEOF    BlazeErr = -258
	blazeErrInvalidDwarf     BlazeErr = -259
	blazeErrOther            BlazeErr = -260
)

func (e BlazeErr) Error() error {
	return errors.New(C.GoString(C.blaze_err_str(C.enum_blaze_err(e))))
}

// NewSymbolizer returns a new Blazesym symbolizer.
func NewSymbolizer() (*Symbolizer, error) {
	s := C.blaze_symbolizer_new()
	if s == nil {
		return nil, fmt.Errorf("failed to create symbolizer")
	}
	return &Symbolizer{s: s}, nil
}

// Symbol represents a symbol from the Blazesym symbolizer.
type Symbol struct {
	Name   string
	File   string
	Dir    string
	Line   int64
	Column int64
	Offset int64
}

// UnknownSymbol returns a symbol representing an unknown symbol.
var UnknownSymbol = Symbol{Name: unknownSymbol}

func stackToPtr(stack []uint64) (*C.uint64_t, C.size_t) {
	var result *C.uint64_t
	length := len(stack)
	if length > 0 {
		result = (*C.uint64_t)(unsafe.Pointer(&stack[0]))
	}
	return result, C.size_t(length)
}

// Symbolize symbolizes an address using the Blazesym symbolizer.
func (s *Symbolizer) Symbolize(pid uint32, stack []uint64) ([]Symbol, error) {
	caddr, clen := stackToPtr(stack)
	symSrcProcess := C.struct_blaze_symbolize_src_process{}
	symSrcProcess.type_size = C.ulong(unsafe.Sizeof(symSrcProcess))
	symSrcProcess.pid = C.uint32_t(pid)
	symSrcProcess.debug_syms = C.bool(false) // for golang not use debug syms?
	symSrcProcess.map_files = C.bool(true)
	symSrcProcess.perf_map = C.bool(true)
	syms := C.blaze_symbolize_process_abs_addrs(s.s, &symSrcProcess, caddr, clen)
	lastErr := BlazeErr(C.blaze_err_last())
	if lastErr != blazeErrOk {
		return []Symbol{UnknownSymbol}, lastErr.Error()
	}
	if syms == nil {
		return []Symbol{UnknownSymbol}, fmt.Errorf("got nil pointer from symbolizer")
	}
	defer C.blaze_syms_free(syms)
	if syms.cnt == 0 {
		return []Symbol{UnknownSymbol}, nil
	}
	var results []Symbol
	for i := 0; i < len(stack); i++ {
		if stack[i] == 0 {
			continue
		}
		if syms == nil || int(syms.cnt) <= i {
			line := "<no-symbol>"
			symbol := Symbol{Name: line, Offset: int64(stack[i])}
			results = append(results, symbol)
			continue
		}

		sym := C.get_result(syms, C.size_t(i))
		name := C.GoString(sym.name)
		if name == "" {
			line := fmt.Sprintf("%x: <no-symbol>", stack[i])
			symbol := Symbol{Name: line, Offset: int64(stack[i])}
			results = append(results, symbol)
			continue
		}
		offset := int64(sym.offset)
		dir := C.GoString(sym.code_info.dir)
		file := C.GoString(sym.code_info.file)
		line := int64(sym.code_info.line)
		column := int64(sym.code_info.column)
		symbol := Symbol{name, file, dir, line, column, offset}
		results = append(results, symbol)
	}
	return results, nil
}

// Close closes the Blazesym symbolizer.
func (s *Symbolizer) Close() {
	C.blaze_symbolizer_free(s.s)
}
