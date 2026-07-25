package config

import "testing"

func TestNormalizeRolesChatFromFlag(t *testing.T) {
	c := &Config{Models: []ModelConfig{
		{ID: "zeta", Endpoint: "e"},
		{ID: "alpha", Endpoint: "e", Chat: true},
	}}
	c.normalizeRoles()
	if c.Roles.Chat.Primary != "alpha" {
		t.Fatalf("chat=true model should win: got %q", c.Roles.Chat.Primary)
	}
}

func TestNormalizeRolesChatFromRolelessLexOrder(t *testing.T) {
	c := &Config{Models: []ModelConfig{
		{ID: "zeta", Endpoint: "e"},
		{ID: "alpha", Endpoint: "e"},
		{ID: "small", Endpoint: "e", Role: ModelSlotSmall},
	}}
	c.normalizeRoles()
	// No chat flag → first role-less in lex order ("alpha"); the
	// role-tagged "small" is excluded.
	if c.Roles.Chat.Primary != "alpha" {
		t.Fatalf("expected role-less lex-first 'alpha', got %q", c.Roles.Chat.Primary)
	}
}

func TestNormalizeRolesExplicitWins(t *testing.T) {
	c := &Config{
		Models: []ModelConfig{
			{ID: "big", Endpoint: "e", Chat: true},
			{ID: "pinned", Endpoint: "e"},
		},
		Roles: RolesConfig{Chat: RoleChain{Primary: "pinned", Backup: "big"}},
	}
	c.normalizeRoles()
	if c.Roles.Chat.Primary != "pinned" || c.Roles.Chat.Backup != "big" {
		t.Fatalf("explicit [roles.chat] must not be overwritten: got %+v", c.Roles.Chat)
	}
}

func TestNormalizeRolesFromSidecarTaskModels(t *testing.T) {
	c := &Config{
		Models: []ModelConfig{
			{ID: "gemma-e4b", Endpoint: "e"},
			{ID: "gemma-31b", Endpoint: "e"},
		},
		Sidecar: SidecarConfig{
			DefaultModel:   "gemma-e4b",
			ExtractModel:   "gemma-31b",
			SummarizeModel: "gemma-31b",
		},
	}
	c.normalizeRoles()
	// classify has no explicit key → default_model.
	if c.Roles.Classify.Primary != "gemma-e4b" {
		t.Fatalf("classify should fall to default_model: got %q", c.Roles.Classify.Primary)
	}
	// extract has an explicit key.
	if c.Roles.Extract.Primary != "gemma-31b" {
		t.Fatalf("extract should use extract_model: got %q", c.Roles.Extract.Primary)
	}
	if c.Roles.Summarize.Primary != "gemma-31b" {
		t.Fatalf("summarize should use summarize_model: got %q", c.Roles.Summarize.Primary)
	}
}

func TestNormalizeRolesFromLegacySlotTags(t *testing.T) {
	c := &Config{Models: []ModelConfig{
		{ID: "small-m", Endpoint: "e", Role: ModelSlotSmall},
		{ID: "medium-m", Endpoint: "e", Role: ModelSlotMedium},
	}}
	c.normalizeRoles()
	// Critical-path tasks → small; background → medium; extract → async
	// (falls to medium since no small_async).
	if c.Roles.Classify.Primary != "small-m" {
		t.Fatalf("classify should map to small slot: got %q", c.Roles.Classify.Primary)
	}
	if c.Roles.Summarize.Primary != "medium-m" {
		t.Fatalf("summarize should map to medium slot: got %q", c.Roles.Summarize.Primary)
	}
	if c.Roles.Extract.Primary != "medium-m" {
		t.Fatalf("extract should fall async→medium: got %q", c.Roles.Extract.Primary)
	}
}

func TestNormalizeRolesClassifierAliasesSmall(t *testing.T) {
	c := &Config{Models: []ModelConfig{
		{ID: "clf", Endpoint: "e", Role: ModelRoleClassifier},
	}}
	c.normalizeRoles()
	if c.Roles.Classify.Primary != "clf" {
		t.Fatalf("legacy classifier role should fill the small-slot tasks: got %q", c.Roles.Classify.Primary)
	}
}

func TestNormalizeEmbedderRolePromotesLegacyBlock(t *testing.T) {
	c := &Config{
		Embedder: EmbedderConfig{Endpoint: "http://e:8100", Model: "nomic-embed-text", Dimension: 768},
	}
	c.normalizeRoles()
	if c.Roles.Embedder.Primary != legacyEmbedderModelID {
		t.Fatalf("embedder role should point at the synthetic model: got %q", c.Roles.Embedder.Primary)
	}
	var found *ModelConfig
	for i := range c.Models {
		if c.Models[i].ID == legacyEmbedderModelID {
			found = &c.Models[i]
		}
	}
	if found == nil {
		t.Fatal("expected a synthesized embeddings [[models]] entry")
	}
	if found.Provider != "embeddings" || found.Endpoint != "http://e:8100" ||
		found.ServedName() != "nomic-embed-text" || found.Dimension != 768 {
		t.Fatalf("synthesized embedder model has wrong fields: %+v", *found)
	}
}

func TestNormalizeEmbedderRoleNoEndpointNoop(t *testing.T) {
	c := &Config{}
	c.normalizeRoles()
	if !c.Roles.Embedder.IsEmpty() {
		t.Fatalf("no [embedder].endpoint → embedder role should stay empty, got %+v", c.Roles.Embedder)
	}
	for _, m := range c.Models {
		if m.ID == legacyEmbedderModelID {
			t.Fatal("should not synthesize an embedder model when no endpoint is set")
		}
	}
}

func TestNormalizeRolesResearchPins(t *testing.T) {
	c := &Config{
		Models: []ModelConfig{{ID: "big", Endpoint: "e", Chat: true}, {ID: "w", Endpoint: "e"}},
		Skills: SkillsConfig{Research: ResearchConfig{WorkerModel: "w", WriterModel: "big"}},
	}
	c.normalizeRoles()
	if c.Roles.ResearchWorker.Primary != "w" {
		t.Fatalf("research worker pin should fold to role: got %q", c.Roles.ResearchWorker.Primary)
	}
	if c.Roles.ResearchWriter.Primary != "big" {
		t.Fatalf("research writer pin should fold to role: got %q", c.Roles.ResearchWriter.Primary)
	}
}

func TestNormalizeRolesTuningDefaults(t *testing.T) {
	c := &Config{}
	c.normalizeRoles()
	if c.Roles.HealthIntervalSecs != DefaultHealthIntervalSecs ||
		c.Roles.FailThreshold != DefaultFailThreshold ||
		c.Roles.RecoverThreshold != DefaultRecoverThreshold {
		t.Fatalf("tuning defaults not applied: %+v", c.Roles)
	}
}

func TestResolvedChainsAppendsFallback(t *testing.T) {
	r := RolesConfig{
		Fallback: "fb",
		Chat:     RoleChain{Primary: "p", Backup: "b"},
	}
	got := r.ResolvedChains()["chat"]
	want := []string{"p", "b", "fb"}
	if len(got) != len(want) {
		t.Fatalf("chat chain: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chat chain[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestResolvedChainsDedupesFallbackEqualToPrimary(t *testing.T) {
	// A role whose only model IS the global fallback shouldn't list it
	// twice.
	r := RolesConfig{Fallback: "only", Classify: RoleChain{Primary: "only"}}
	got := r.ResolvedChains()["classify"]
	if len(got) != 1 || got[0] != "only" {
		t.Fatalf("expected deduped single-element chain, got %v", got)
	}
}

// Validation of the [roles] chains.
func TestValidateRejectsUnknownRoleModel(t *testing.T) {
	c := DefaultConfig()
	c.Models = []ModelConfig{{ID: "real", Endpoint: "e"}}
	c.Roles.Chat = RoleChain{Primary: "real", Backup: "ghost"}
	c.normalizeRoles()
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for backup referencing a missing model")
	}
}

func TestValidateRejectsEmbedderDimMismatch(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{} // disable legacy shim
	c.Models = []ModelConfig{
		{ID: "ea", Endpoint: "e", Provider: "embeddings", Model: "nomic", Dimension: 768},
		{ID: "eb", Endpoint: "e", Provider: "embeddings", Model: "nomic", Dimension: 1024},
	}
	c.Roles.Embedder = RoleChain{Primary: "ea", Backup: "eb"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for embedder backup with mismatched dimension")
	}
}

func TestValidateRejectsEmbedderModelMismatch(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{
		{ID: "ea", Endpoint: "e", Provider: "embeddings", Model: "nomic", Dimension: 768},
		{ID: "eb", Endpoint: "e", Provider: "embeddings", Model: "bge", Dimension: 768},
	}
	c.Roles.Embedder = RoleChain{Primary: "ea", Backup: "eb"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for embedder backup with a different model family")
	}
}

func TestValidateAcceptsMatchedEmbedderChain(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{
		{ID: "ea", Endpoint: "e", Provider: "embeddings", Model: "nomic", Dimension: 768},
		{ID: "eb", Endpoint: "e", Provider: "embeddings", Model: "nomic", Dimension: 768},
		{ID: "chat", Endpoint: "e", Chat: true},
	}
	c.Roles.Embedder = RoleChain{Primary: "ea", Backup: "eb"}
	c.normalizeRoles()
	if err := c.Validate(); err != nil {
		t.Fatalf("matched embedder chain should validate: %v", err)
	}
}

// Pointing the embedder role at a chat model would silently drop every
// vector — reject it at load.
func TestValidateRejectsNonEmbeddingsEmbedder(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{
		{ID: "chatty", Endpoint: "e", Provider: "llama-server", Chat: true},
	}
	c.Roles.Embedder = RoleChain{Primary: "chatty"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error when the embedder role points at a chat provider")
	}
}

// The legacy [embedder] block must survive a full Load+Validate and come
// out as a health-checked embeddings model in the embedder chain.
func TestLegacyEmbedderBlockLoadsAndValidates(t *testing.T) {
	c := DefaultConfig()
	c.Models = []ModelConfig{{ID: "chat", Endpoint: "e", Provider: "llama-server", Chat: true}}
	c.Embedder = EmbedderConfig{Endpoint: "http://127.0.0.1:8100", Model: "nomic-embed-text", Dimension: 768}
	c.normalizeRoles()
	if err := c.Validate(); err != nil {
		t.Fatalf("legacy embedder config should validate: %v", err)
	}
	chain := c.Roles.ResolvedChains()[RoleEmbedder]
	if len(chain) != 1 || chain[0] != legacyEmbedderModelID {
		t.Fatalf("embedder chain = %v, want [%s]", chain, legacyEmbedderModelID)
	}
}

// UPGRADE SAFETY: a legacy [sidecar].*_model naming a model that isn't
// in [[models]] used to warn and skip that task. It must not become a
// boot failure — an operator upgrading with that mismatch has a config
// that ran yesterday.
func TestDerivedRoleWithUnknownModelIsNotFatal(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{{ID: "chat", Endpoint: "e", Provider: "llama-server", Chat: true}}
	c.Sidecar = SidecarConfig{Enabled: true, ClassifyModel: "sidecar/typo"}
	c.normalizeRoles()

	if err := c.Validate(); err != nil {
		t.Fatalf("a legacy typo must stay non-fatal on upgrade, got: %v", err)
	}
	// And the bad candidate is pruned, so the resolver never hands the
	// sidecar a model the registry can't resolve.
	if c.Roles.Classify.Primary != "" {
		t.Errorf("unknown derived candidate should be pruned, got %q", c.Roles.Classify.Primary)
	}
	if _, present := c.Roles.ResolvedChains()[RoleClassify]; present {
		t.Error("a pruned role should not appear in the resolved chains")
	}
}

// ...but a hand-written [roles] typo IS fatal: that's new config the
// operator just wrote, and silently skipping it would hide the mistake.
func TestExplicitRoleWithUnknownModelIsFatal(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{{ID: "chat", Endpoint: "e", Provider: "llama-server", Chat: true}}
	c.Roles.Classify = RoleChain{Primary: "sidecar/typo"}
	c.normalizeRoles()

	if err := c.Validate(); err == nil {
		t.Fatal("an explicit [roles] typo should fail validation")
	}
}

// When only a derived primary is missing, a valid backup is promoted
// rather than the whole role being dropped.
func TestDerivedBackupPromotedWhenPrimaryPruned(t *testing.T) {
	c := DefaultConfig()
	c.Embedder = EmbedderConfig{}
	c.Models = []ModelConfig{
		{ID: "chat", Endpoint: "e", Provider: "llama-server", Chat: true},
		{ID: "real", Endpoint: "e", Provider: "llama-server"},
	}
	// Simulate a derived chain (not in explicitRoles) with a bad primary.
	c.normalizeRoles()
	c.Roles.Summarize = RoleChain{Primary: "ghost", Backup: "real"}
	c.pruneDerivedRoles()

	if c.Roles.Summarize.Primary != "real" || c.Roles.Summarize.Backup != "" {
		t.Fatalf("expected the backup promoted to primary, got %+v", c.Roles.Summarize)
	}
}
