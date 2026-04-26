package host

import "context"

type HostSource interface {
	ID() string
	Start(ctx context.Context, sink HostSink) error
	Stop(ctx context.Context) error
	Close() error
}

type HostSink interface {
	EmitLaunchRecord(record LaunchRecord) error
}
