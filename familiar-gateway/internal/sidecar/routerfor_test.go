package sidecar

// routerFor returns the HTTPRouter for a task's currently-resolved
// endpoint regardless of health, or nil when the task names no model.
// Tests use it to inspect the router a task would get.
func (c *Client) routerFor(task string) *HTTPRouter {
	if c.roles == nil {
		return nil
	}
	modelID, _, ok := c.roles.Resolve(task)
	if !ok || modelID == "" {
		return nil
	}
	ep := c.endpointFor(modelID)
	if ep == "" {
		return nil
	}
	return c.routerForEndpoint(ep, c.requestModelFor(modelID), task == TaskExtractLarge)
}
