package main

// Placeholder for the Cloud Logging session viewer; replaced by the real one.
type cloudSessionPlaceholder struct{}

func (cloudSessionPlaceholder) handleEvent(event)     {}
func (cloudSessionPlaceholder) handleKey([]byte) bool { return true }
func (cloudSessionPlaceholder) draw()                 {}
func (cloudSessionPlaceholder) leave(bool)            {}

func newCloudSession(p *pane, spec cloudSessionSpec, back func()) screen {
	return cloudSessionPlaceholder{}
}
