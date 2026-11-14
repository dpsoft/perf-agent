// Package profilemodel is an offline model of a pprof profile: it loads a
// profile and, optionally, its GPU executions log, and later layers build a
// node tree, facts, quality indicators and a diff on top of it.
//
// Facts come from the profile and from a checked log only. Nothing is
// inferred, estimated or scaled: values stay in the profile's sample unit as
// exact integers, and a log that does not agree with the profile is reported
// as unavailable rather than partly trusted.
//
// # Limits
//
// Every input is bounded. DefaultLimits are:
//
//	MaxRawBytes          256 MiB   compressed (as read) profile size
//	MaxDecompressedBytes 1 GiB     inflated profile size
//	MaxSamples           5,000,000 samples
//	MaxLocations         5,000,000 locations
//	MaxDepth             8192      locations in one sample's stack
//	MaxSidecarBytes      256 MiB   executions log size
//	MaxSidecarRows       10,000,000 log rows
//	MaxSidecarExecutions 10,000,000 distinct execution ordinals
//
// They sit far above what the agent produces (a long capture yields well
// under a million samples and stacks of a few hundred frames) and low enough
// that a hostile file cannot exhaust memory. A profile over a limit is
// refused with an error wrapping ErrLimit; a log over a limit only disables
// the executions facts.
package profilemodel
