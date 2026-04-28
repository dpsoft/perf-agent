package hip

import "errors"

var (
	errPIDRequired         = errors.New("pid is required")
	errLibraryPathRequired = errors.New("library path is required")
	errSymbolRequired      = errors.New("symbol is required")
)

type Config struct {
	PID         int
	LibraryPath string
	Symbol      string

	testRecords []rawRecord
	testDecode  func(rawRecord) (launchRecord, error)
}

func (c Config) validate() error {
	if c.PID <= 0 {
		return errPIDRequired
	}
	if len(c.testRecords) == 0 && c.testDecode == nil {
		if c.LibraryPath == "" {
			return errLibraryPathRequired
		}
		if c.Symbol == "" {
			return errSymbolRequired
		}
	}
	return nil
}
