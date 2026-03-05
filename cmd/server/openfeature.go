//go:build goexperiment.jsonv2

package main

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
)

// openFeatureConfig holds OpenFeature configuration
type openFeatureConfig struct {
	enabled bool
	client  *openfeature.Client
	cacheTTL time.Duration
}

var openFeatureCfg openFeatureConfig

// initOpenFeature initializes OpenFeature if configured.
// Returns true if OpenFeature is enabled, false otherwise.
func initOpenFeature() bool {
	providerType := os.Getenv("OPENFEATURE_PROVIDER")
	if providerType == "" {
		return false
	}

	var provider openfeature.FeatureProvider
	var err error

	switch providerType {
	case "envvar":
		provider = newEnvVarProvider()
	case "inmemory":
		provider = newInMemoryProvider()
	case "http":
		provider, err = newHTTPProvider()
		if err != nil {
			return false
		}
	default:
		return false
	}

	err = openfeature.SetProviderAndWait(provider)
	if err != nil {
		return false
	}
	openFeatureCfg.client = openfeature.NewClient("ssfbff")
	openFeatureCfg.enabled = true

	// Parse cache TTL from environment variable (used as fallback when events not available)
	cacheTTLStr := os.Getenv("OPENFEATURE_CACHE_TTL")
	if cacheTTLStr != "" {
		if cacheTTLSeconds, err := strconv.Atoi(cacheTTLStr); err == nil && cacheTTLSeconds > 0 {
			openFeatureCfg.cacheTTL = time.Duration(cacheTTLSeconds) * time.Second
		}
	}

	// Set up event handlers for push/streaming updates (priority over TTL)
	setupOpenFeatureEventHandlers(provider)

	return true
}

// setupOpenFeatureEventHandlers sets up event handlers to receive push/streaming updates from providers.
// When flag changes are detected via events, the cache is invalidated immediately.
func setupOpenFeatureEventHandlers(provider openfeature.FeatureProvider) {
	// Check if provider supports eventing
	eventHandler, supportsEvents := provider.(openfeature.EventHandler)
	if !supportsEvents {
		// Provider doesn't support push/streaming, will use TTL fallback
		return
	}

	// Start goroutine to listen for events
	eventChan := eventHandler.EventChannel()
	go func() {
		for event := range eventChan {
			handleOpenFeatureEvent(event)
		}
	}()

	// Register global event handlers for provider configuration changes
	configChangeCallback := func(details openfeature.EventDetails) {
		handleOpenFeatureEvent(openfeature.Event{
			ProviderName: details.ProviderName,
			EventType:    openfeature.ProviderConfigChange,
			ProviderEventDetails: details.ProviderEventDetails,
		})
	}
	var callback openfeature.EventCallback = &configChangeCallback
	openfeature.AddHandler(openfeature.ProviderConfigChange, callback)
}

// handleOpenFeatureEvent processes OpenFeature events and invalidates cache for changed flags.
func handleOpenFeatureEvent(event openfeature.Event) {
	if event.ProviderEventDetails.FlagChanges == nil || len(event.ProviderEventDetails.FlagChanges) == 0 {
		return
	}

	// Invalidate cache for changed flags (push/streaming update)
	envCache.openFeatureCache.mu.Lock()
	for _, flagKey := range event.ProviderEventDetails.FlagChanges {
		delete(envCache.openFeatureCache.entries, flagKey)
	}
	envCache.openFeatureCache.mu.Unlock()
}

// evaluateFlagString evaluates a string flag from OpenFeature.
// Returns the flag value and true if found, or empty string and false if not found.
func evaluateFlagString(ctx context.Context, flagKey string) (string, bool) {
	if !openFeatureCfg.enabled || openFeatureCfg.client == nil {
		return "", false
	}

	evalCtx := openfeature.NewTargetlessEvaluationContext(map[string]interface{}{})
	value, err := openFeatureCfg.client.StringValue(ctx, flagKey, "", evalCtx)
	if err != nil {
		return "", false
	}
	
	return value, true
}

// evaluateFlagInt evaluates an integer flag from OpenFeature.
// Returns the flag value and true if found, or 0 and false if not found.
func evaluateFlagInt(ctx context.Context, flagKey string) (int, bool) {
	if !openFeatureCfg.enabled || openFeatureCfg.client == nil {
		return 0, false
	}

	evalCtx := openfeature.NewTargetlessEvaluationContext(map[string]interface{}{})
	value, err := openFeatureCfg.client.IntValue(ctx, flagKey, 0, evalCtx)
	if err != nil {
		return 0, false
	}

	return int(value), true
}

// evaluateFlagBool evaluates a boolean flag from OpenFeature.
// Returns the flag value and true if found, or false and false if not found.
func evaluateFlagBool(ctx context.Context, flagKey string) (bool, bool) {
	if !openFeatureCfg.enabled || openFeatureCfg.client == nil {
		return false, false
	}

	evalCtx := openfeature.NewTargetlessEvaluationContext(map[string]interface{}{})
	value, err := openFeatureCfg.client.BooleanValue(ctx, flagKey, false, evalCtx)
	if err != nil {
		return false, false
	}

	return value, true
}

// evaluateFlagFloat evaluates a float flag from OpenFeature.
// Returns the flag value and true if found, or 0.0 and false if not found.
func evaluateFlagFloat(ctx context.Context, flagKey string) (float64, bool) {
	if !openFeatureCfg.enabled || openFeatureCfg.client == nil {
		return 0.0, false
	}

	evalCtx := openfeature.NewTargetlessEvaluationContext(map[string]interface{}{})
	value, err := openFeatureCfg.client.FloatValue(ctx, flagKey, 0.0, evalCtx)
	if err != nil {
		return 0.0, false
	}

	return value, true
}

// Simple provider implementations

// envVarProvider reads flags from environment variables
type envVarProvider struct{}

func newEnvVarProvider() openfeature.FeatureProvider {
	return &envVarProvider{}
}

func (p *envVarProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "envvar"}
}

func (p *envVarProvider) Hooks() []openfeature.Hook {
	return []openfeature.Hook{}
}

func (p *envVarProvider) BooleanEvaluation(ctx context.Context, flag string, defaultValue bool, evalCtx openfeature.FlattenedContext) openfeature.BoolResolutionDetail {
	val := os.Getenv(flag)
	if val == "" {
		return openfeature.BoolResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
	}
	result, err := parseBool(val)
	if err != nil {
		return openfeature.BoolResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.ErrorReason}}
	}
	return openfeature.BoolResolutionDetail{Value: result, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
}

func (p *envVarProvider) StringEvaluation(ctx context.Context, flag string, defaultValue string, evalCtx openfeature.FlattenedContext) openfeature.StringResolutionDetail {
	val := os.Getenv(flag)
	if val == "" {
		return openfeature.StringResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
	}
	return openfeature.StringResolutionDetail{Value: val, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
}

func (p *envVarProvider) FloatEvaluation(ctx context.Context, flag string, defaultValue float64, evalCtx openfeature.FlattenedContext) openfeature.FloatResolutionDetail {
	val := os.Getenv(flag)
	if val == "" {
		return openfeature.FloatResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
	}
	result, err := parseFloat(val)
	if err != nil {
		return openfeature.FloatResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.ErrorReason}}
	}
	return openfeature.FloatResolutionDetail{Value: result, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
}

func (p *envVarProvider) IntEvaluation(ctx context.Context, flag string, defaultValue int64, evalCtx openfeature.FlattenedContext) openfeature.IntResolutionDetail {
	val := os.Getenv(flag)
	if val == "" {
		return openfeature.IntResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
	}
	result, err := parseInt(val)
	if err != nil {
		return openfeature.IntResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.ErrorReason}}
	}
	return openfeature.IntResolutionDetail{Value: result, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
}

func (p *envVarProvider) ObjectEvaluation(ctx context.Context, flag string, defaultValue interface{}, evalCtx openfeature.FlattenedContext) openfeature.InterfaceResolutionDetail {
	return openfeature.InterfaceResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
}

// inMemoryProvider is a simple in-memory provider (can be extended with actual flag storage)
type inMemoryProvider struct {
	flags map[string]interface{}
}

func newInMemoryProvider() openfeature.FeatureProvider {
	return &inMemoryProvider{
		flags: make(map[string]interface{}),
	}
}

func (p *inMemoryProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "inmemory"}
}

func (p *inMemoryProvider) Hooks() []openfeature.Hook {
	return []openfeature.Hook{}
}

func (p *inMemoryProvider) BooleanEvaluation(ctx context.Context, flag string, defaultValue bool, evalCtx openfeature.FlattenedContext) openfeature.BoolResolutionDetail {
	if val, ok := p.flags[flag].(bool); ok {
		return openfeature.BoolResolutionDetail{Value: val, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
	}
	return openfeature.BoolResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
}

func (p *inMemoryProvider) StringEvaluation(ctx context.Context, flag string, defaultValue string, evalCtx openfeature.FlattenedContext) openfeature.StringResolutionDetail {
	if val, ok := p.flags[flag].(string); ok {
		return openfeature.StringResolutionDetail{Value: val, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
	}
	return openfeature.StringResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
}

func (p *inMemoryProvider) FloatEvaluation(ctx context.Context, flag string, defaultValue float64, evalCtx openfeature.FlattenedContext) openfeature.FloatResolutionDetail {
	if val, ok := p.flags[flag].(float64); ok {
		return openfeature.FloatResolutionDetail{Value: val, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
	}
	return openfeature.FloatResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
}

func (p *inMemoryProvider) IntEvaluation(ctx context.Context, flag string, defaultValue int64, evalCtx openfeature.FlattenedContext) openfeature.IntResolutionDetail {
	if val, ok := p.flags[flag].(int64); ok {
		return openfeature.IntResolutionDetail{Value: val, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
	}
	return openfeature.IntResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
}

func (p *inMemoryProvider) ObjectEvaluation(ctx context.Context, flag string, defaultValue interface{}, evalCtx openfeature.FlattenedContext) openfeature.InterfaceResolutionDetail {
	if val, ok := p.flags[flag].(map[string]interface{}); ok {
		return openfeature.InterfaceResolutionDetail{Value: val, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}}
	}
	return openfeature.InterfaceResolutionDetail{Value: defaultValue, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason}}
}

// httpProvider placeholder - would need actual HTTP provider implementation
func newHTTPProvider() (openfeature.FeatureProvider, error) {
	return nil, nil
}

// Helper functions for parsing
func parseBool(s string) (bool, error) {
	return strconv.ParseBool(s)
}

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func parseInt(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
