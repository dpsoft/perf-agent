// Vendor clock to CPU-monotonic conversion.
//
// The vendor clock is not a drifting device clock: cuptiGetTimestamp is
// CLOCK_REALTIME (spec §7, measured 2000/2000 bracketed). So the conversion
// is an offset between two host clocks, and the hazard is a REALTIME step -
// by NTP, an administrator, or a container host - which shifts every GPU
// timestamp at once and can place an execution before its own launch.
// Slew is absorbed; a step re-anchors and is counted.
#ifndef PERFAGENT_CLOCK_H
#define PERFAGENT_CLOCK_H

#include <cstdint>

namespace perfagent {

// Offset movement beyond this between consecutive samples is a step, not
// slew. 1ms is far above observed NTP slew (<15us over 20s) and far below
// any step worth worrying about.
constexpr int64_t kStepThresholdNs = 1'000'000;

class ClockFit {
public:
    void resample(uint64_t vendor_ns, uint64_t mono_ns) {
        const int64_t offset = (int64_t)vendor_ns - (int64_t)mono_ns;
        if (valid_) {
            const int64_t delta = offset - offset_;
            const int64_t mag = delta < 0 ? -delta : delta;
            if (mag > kStepThresholdNs) steps_++;
        }
        offset_ = offset;
        valid_ = true;
    }

    uint64_t to_monotonic(uint64_t vendor_ns) const {
        if (!valid_) return 0;
        return (uint64_t)((int64_t)vendor_ns - offset_);
    }

    bool valid() const { return valid_; }
    uint64_t steps() const { return steps_; }
    int64_t offset_ns() const { return offset_; }

private:
    int64_t offset_ = 0;
    bool valid_ = false;
    uint64_t steps_ = 0;
};

}  // namespace perfagent

#endif
