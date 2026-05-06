package debuginfod

import "errors"

var (
	ErrClosed      = errors.New("debuginfod: closed")
	ErrNoURLs      = errors.New("debuginfod: no URLs configured")
	ErrInvalidOpts = errors.New("debuginfod: invalid options")
)
