package mp4

// Fixtures of this package's tests, for the external test package (it needs
// mkv/ops, which imports this package).
var (
	BuildSubCursorFixtureForTest = buildSubCursorFixture
	BuildSeekFixtureForTest      = buildSeekFixture
)

// DisableSubHarvestForTest makes p walk the clusters for its subtitle cues as it did before the window walks
// harvested them: the reference the harvested cues are compared with.
func DisableSubHarvestForTest(p *HLSPlan) { p.harvest.off = true }
