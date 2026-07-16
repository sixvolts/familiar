package config

import (
	"fmt"
	"log"
	"strings"
)

// Validate sanity-checks a loaded Config and reports the first problem
// it finds as an error. The checks catch the class of bugs that
// otherwise surface as mysterious runtime failures: zero window sizes,
// out-of-range ratios, unreachable prompt files, unparseable cron
// expressions, or models with missing endpoints.
//
// Callers should invoke Validate immediately after Load, before wiring
// any subsystems. Returning early here is cheaper than debugging a
// half-initialized gateway.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}

	// Context window budgeting — zero window means division by zero in
	// the ctxbuild assembler, and OutputReservation >= WindowSize leaves
	// no budget for prompt content at all.
	if c.Context.WindowSize <= 0 {
		return fmt.Errorf("context.window_size must be > 0 (got %d)", c.Context.WindowSize)
	}
	if c.Context.OutputReservation < 0 {
		return fmt.Errorf("context.output_reservation must be >= 0 (got %d)", c.Context.OutputReservation)
	}
	if c.Context.OutputReservation >= c.Context.WindowSize {
		return fmt.Errorf("context.output_reservation (%d) must be < context.window_size (%d)",
			c.Context.OutputReservation, c.Context.WindowSize)
	}
	ratios := map[string]float64{
		"context.system_prompt_ratio": c.Context.SystemPromptRatio,
		"context.memory_ratio":        c.Context.MemoryRatio,
		"context.tool_result_ratio":   c.Context.ToolResultRatio,
	}
	var ratioSum float64
	for name, r := range ratios {
		if r < 0 || r > 1 {
			return fmt.Errorf("%s must be in [0,1] (got %v)", name, r)
		}
		ratioSum += r
	}
	if ratioSum > 1.0 {
		return fmt.Errorf("context zone ratios sum to %.2f; must be <= 1.0", ratioSum)
	}

	// Memory thresholds — similarity scores live in [0,1]; a threshold
	// outside that range means "always" or "never" match silently.
	if c.Memory.RelevanceThreshold < 0 || c.Memory.RelevanceThreshold > 1 {
		return fmt.Errorf("memory.relevance_threshold must be in [0,1] (got %v)", c.Memory.RelevanceThreshold)
	}
	if c.Memory.DedupThreshold < 0 || c.Memory.DedupThreshold > 1 {
		return fmt.Errorf("memory.dedup_threshold must be in [0,1] (got %v)", c.Memory.DedupThreshold)
	}
	if c.Memory.DedupThreshold > 0 && c.Memory.DedupThreshold < c.Memory.RelevanceThreshold {
		return fmt.Errorf("memory.dedup_threshold (%v) must be >= memory.relevance_threshold (%v)",
			c.Memory.DedupThreshold, c.Memory.RelevanceThreshold)
	}

	// At least one model must be configured and each entry must have an
	// id and endpoint. An empty Models slice would leave the router
	// unable to select any provider at runtime.
	if len(c.Models) == 0 {
		return fmt.Errorf("at least one model must be configured")
	}
	modelIDs := make(map[string]struct{}, len(c.Models))
	roleClaim := make(map[string]string) // role → model ID
	for i, m := range c.Models {
		if m.ID == "" {
			return fmt.Errorf("models[%d]: id is required", i)
		}
		if m.Endpoint == "" {
			return fmt.Errorf("models[%d] (%s): endpoint is required", i, m.ID)
		}
		modelIDs[m.ID] = struct{}{}

		// Role validation. Reject unknown role strings so a typo
		// ("classifyer") doesn't silently leave the slot unfilled.
		// "classifier" is accepted as a legacy alias for "small".
		if m.Role != "" {
			switch m.Role {
			case ModelSlotSmall, ModelSlotMedium, ModelSlotSmallAsync,
				ModelRoleEmbedder, ModelRoleClassifier:
				// ok
			default:
				return fmt.Errorf("models[%d] (%s): unknown role %q (want %q / %q / %q / %q)",
					i, m.ID, m.Role, ModelSlotSmall, ModelSlotMedium, ModelSlotSmallAsync, ModelRoleEmbedder)
			}
			canon := m.Role
			if canon == ModelRoleClassifier {
				canon = ModelSlotSmall
			}
			if prior, conflict := roleClaim[canon]; conflict {
				return fmt.Errorf("models[%d] (%s): role %q already claimed by %q (only one model per role)",
					i, m.ID, canon, prior)
			}
			roleClaim[canon] = m.ID
		}
	}

	// [engine] is no longer validated
	// deleted the previous engine and the gRPC dial. The struct stays
	// as an empty block so existing gateway.toml entries parse
	// cleanly; nothing inside is read.

	// Sidecar routing (SIDECAR-SLOT-FIXES.md). An enabled sidecar
	// needs *some* way to resolve task endpoints. Accept any of:
	//   - an explicit task → model assignment ([sidecar].*_model or
	//     default_model),
	//   - a [[models]] entry with role="small" (legacy role path),
	//   - a literal sidecar.router_endpoint (legacy).
	// router_endpoint is no longer required on its own.
	_, hasSmallRole := roleClaim[ModelSlotSmall]
	if c.Sidecar.Enabled &&
		!c.Sidecar.HasExplicitTaskModels() &&
		!hasSmallRole &&
		c.Sidecar.RouterEndpoint == "" {
		return fmt.Errorf("sidecar.enabled=true but no routing is configured: " +
			"set [sidecar].default_model (or per-task *_model), " +
			`assign role="small" to a [[models]] entry, ` +
			"or set sidecar.router_endpoint")
	}

	// A task → model assignment that points at a model ID absent from
	// [[models]] resolves to no endpoint and the task is silently
	// skipped. Warn (don't error) so a typo surfaces in the log
	// without blocking startup.
	if c.Sidecar.HasExplicitTaskModels() {
		for _, tm := range c.Sidecar.SidecarTaskModels() {
			if tm.Model == "" {
				continue
			}
			if _, ok := modelIDs[tm.Model]; !ok {
				log.Printf("[config] warning: sidecar %s_model = %q has no matching [[models]] entry — task will be skipped",
					tm.Task, tm.Model)
			}
		}
	}

	// extract_large_model is an additive route (kept out of
	// SidecarTaskModels so it never disables the other tasks' fallback),
	// so validate it separately. A typo resolves to no endpoint and the
	// large-extract path falls back to the small extract model.
	if c.Sidecar.ExtractLargeModel != "" {
		if _, ok := modelIDs[c.Sidecar.ExtractLargeModel]; !ok {
			log.Printf("[config] warning: sidecar extract_large_model = %q has no matching [[models]] entry — large extraction will fall back to the extract model",
				c.Sidecar.ExtractLargeModel)
		}
	}

	// Slack adapter is auto-enabled when both tokens are present
	// (see cmd/gateway/main.go). Partial configuration — one token set,
	// the other empty — means the adapter silently never starts, which
	// looks like a deploy bug. Require both or neither.
	slack := c.Adapter.Slack
	if (slack.BotToken != "") != (slack.AppToken != "") {
		return fmt.Errorf("adapter.slack: bot_token and app_token must both be set or both be empty")
	}

	// Admin relying-party shape (PUBLIC-PROXY-MIGRATION). Only
	// validated when the admin subsystem is enabled — disabled
	// deployments don't need WebAuthn config. Catch the cases that
	// otherwise surface as mysterious 400s once a request comes in:
	// no RP configured, an RP with no origins or hosts, or two RPs
	// claiming the same inbound Host header.
	if c.Admin.Enabled {
		rps := c.Admin.EffectiveRelyingParties()
		if len(rps) == 0 {
			return fmt.Errorf("admin: at least one [[admin.relying_party]] block (or legacy rp_id + rp_origins) is required when admin.enabled = true")
		}
		seenHost := make(map[string]string)
		for i, rp := range rps {
			if rp.RPID == "" {
				return fmt.Errorf("admin.relying_party[%d]: rp_id is required", i)
			}
			if len(rp.Origins) == 0 {
				return fmt.Errorf("admin.relying_party[%d] (%s): at least one origin is required", i, rp.RPID)
			}
			if len(rp.Hosts) == 0 {
				return fmt.Errorf("admin.relying_party[%d] (%s): at least one host is required", i, rp.RPID)
			}
			for _, host := range rp.Hosts {
				if host == "" {
					return fmt.Errorf("admin.relying_party[%d] (%s): empty host entry", i, rp.RPID)
				}
				key := strings.ToLower(host)
				if prior, dup := seenHost[key]; dup {
					return fmt.Errorf("admin.relying_party: host %q is claimed by both %q and %q (each inbound Host header must map to exactly one RP)",
						key, prior, rp.RPID)
				}
				seenHost[key] = rp.RPID
			}
		}
	}

	// SystemPrompt.File is intentionally not validated here. main.go
	// already logs a warning and continues with an empty prompt if the
	// file is missing, and the default DefaultConfig() points at
	// ~/.familiar/system_prompt.md — a file that may or may not exist
	// on a fresh install. Forcing it to be readable would break first-run.

	// Scheduled work is DB-backed (SCHEDULED-ACTIONS-SPEC) and
	// validated at write time by the actions store — config no
	// longer carries any schedules to check.

	return nil
}
