package server

func (echoProvider) Efforts() []string                 { return []string{"high"} }
func (echoProvider) ModelEfforts() map[string][]string { return nil }
func (echoProvider) DefaultModel() (string, string)    { return "m", "high" }
