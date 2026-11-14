package profilemodel

// Notes is the view's sampling notes, the text Fold warns with: GPU views
// with a non-zero total only, nil otherwise. They describe the whole view,
// never one node.
func (v *View) Notes() ([]string, error) { return viewSamplingNotes(v) }

// IsGPU reports whether the view is a GPU view.
func (v *View) IsGPU() bool { return v.SampleType == gpuSampleType }

// UnsampledPortion is the view-level value under the unsampled launch
// frame, unavailable "not a GPU view" off GPU. It is never a node's value.
// The error is an overflow.
func (v *View) UnsampledPortion() (Fact, error) {
	if v.SampleType != gpuSampleType {
		return unavailableFact(MetricUnsampledPortion, v.Unit, ReasonNotGPUView), nil
	}
	u, err := v.unsampledTotal()
	if err != nil {
		return Fact{}, err
	}
	return intFact(MetricUnsampledPortion, u, v.Unit), nil
}
