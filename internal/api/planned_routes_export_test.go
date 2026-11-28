package api

// WithPlannedRoutes mounts extra planned GET answers, for tests that exercise
// the planned mechanism while no documented operation is planned. It exists
// only in test builds: production code never sets extraPlanned. It returns h
// for chaining.
func (h *Handler) WithPlannedRoutes(routes ...Route) *Handler {
	h.extraPlanned = routes
	return h
}
