package room

// Effort-related Provider methods for the fake used in hub tests.

func (f *fakeProvider) Efforts() []string                 { return []string{"low", "high"} }
func (f *fakeProvider) ModelEfforts() map[string][]string { return nil }
func (f *fakeProvider) DefaultModel() (string, string)    { return "m", "high" }
