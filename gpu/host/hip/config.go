package hip

import "errors"

var errPIDRequired = errors.New("pid is required")

type Config struct {
	PID int

	testRecords []rawRecord
	testDecode  func(rawRecord) (launchRecord, error)
}

func (c Config) validate() error {
	if c.PID <= 0 {
		return errPIDRequired
	}
	return nil
}
