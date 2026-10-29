package usagehistory

import (
	"regexp"
	"strings"
)

// PricingDate is the verification date of the bundled official USD/MTok list rates.
const PricingDate = "2026-10-29"

// Sources: https://developers.openai.com/api/docs/pricing
// https://platform.claude.com/docs/en/about-claude/pricing
// https://ai.google.dev/gemini-api/docs/pricing
// https://docs.x.ai/developers/pricing
// Estimates cover text tokens, standard cache TTLs, and supported service tiers.
// Tool fees, regional uplifts, storage charges, and subscriptions are excluded.
type price struct {
	input, output, read, write float64
	longAbove                  int64
	longInput, longOutput      float64
	vendor                     string
}

var prices = map[string]price{
	"gpt-6-astra":                        {10, 50, 1, 12.5, 272000, 2, 1.5, "openai"},
	"gpt-6.1-sol":                        {2, 10, .1, 2.5, 272000, 2, 1.5, "openai"},
	"gpt-6-sol":                          {2, 10, .2, 2.5, 272000, 2, 1.5, "openai"},
	"gpt-6.1-terra":                      {1, 5, .05, 1.25, 272000, 2, 1.5, "openai"},
	"gpt-6-luna":                         {.1, .5, .01, .125, 272000, 2, 1.5, "openai"},
	"gpt-5.4":                            {2.5, 15, .25, 0, 272000, 2, 1.5, "openai"},
	"gpt-5.3-codex":                      {1.75, 14, .175, 0, 0, 0, 0, "openai"},
	"claude-fable-5-1":                   {10, 50, .25, 12.5, 0, 0, 0, "anthropic"},
	"claude-fable-5":                     {10, 50, 1, 12.5, 0, 0, 0, "anthropic"},
	"claude-mythos-5-1":                  {10, 50, .25, 12.5, 0, 0, 0, "anthropic"},
	"claude-mythos-5":                    {10, 50, 1, 12.5, 0, 0, 0, "anthropic"},
	"claude-opus-5-5":                    {4, 20, .2, 5, 0, 0, 0, "anthropic"},
	"claude-opus-5":                      {5, 25, .5, 6.25, 0, 0, 0, "anthropic"},
	"claude-opus-4-8":                    {5, 25, .5, 6.25, 0, 0, 0, "anthropic"},
	"claude-opus-4-7":                    {5, 25, .5, 6.25, 0, 0, 0, "anthropic"},
	"claude-opus-4-6":                    {5, 25, .5, 6.25, 0, 0, 0, "anthropic"},
	"claude-opus-4-5":                    {5, 25, .5, 6.25, 0, 0, 0, "anthropic"},
	"claude-sonnet-5-5":                  {2, 10, .1, 2.5, 0, 0, 0, "anthropic"},
	"claude-sonnet-5":                    {2, 10, .2, 2.5, 0, 0, 0, "anthropic"},
	"claude-sonnet-4-6":                  {3, 15, .3, 3.75, 0, 0, 0, "anthropic"},
	"claude-sonnet-4-5":                  {3, 15, .3, 3.75, 200000, 2, 1.5, "anthropic"},
	"claude-haiku-5-5":                   {.1, .5, .01, .125, 100000, 5, 5, "anthropic"},
	"claude-haiku-4-5":                   {1, 5, .1, 1.25, 0, 0, 0, "anthropic"},
	"gemini-4-argon":                     {2, 10, .1, 0, 0, 0, 0, "google"},
	"gemini-3.1-pro-preview":             {2, 12, .2, 0, 200000, 2, 1.5, "google"},
	"gemini-3.1-pro-preview-customtools": {2, 12, .2, 0, 200000, 2, 1.5, "google"},
	"gemini-2.5-pro":                     {1.25, 10, .125, 0, 200000, 2, 1.5, "google"},
	"gemini-2.5-flash":                   {.3, 2.5, .03, 0, 0, 0, 0, "google"},
	"grok-4.7":                           {2, 6, .5, 0, 199999, 2, 2, "xai"},
	"grok-4.6":                           {2, 6, .5, 0, 199999, 2, 2, "xai"},
	"grok-4.5":                           {2, 6, .3, 0, 199999, 2, 2, "xai"},
	"grok-4.3":                           {1.25, 2.5, .2, 0, 199999, 2, 2, "xai"},
	"grok-build-0.1":                     {1, 2, .2, 0, 199999, 2, 2, "xai"},
}

var datedSnapshot = regexp.MustCompile(`-(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{8})$`)

func modelPrice(model string) (price, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if value, ok := prices[model]; ok {
		return value, true
	}
	value, ok := prices[datedSnapshot.ReplaceAllString(model, "")]
	return value, ok
}

func estimateCost(record journalRecord) (float64, bool) {
	tokens := record.Tokens
	if !tokens.Valid() || tokens.UnclassifiedTokens != 0 {
		return 0, false
	}
	if tokens.TotalTokens == 0 {
		return 0, true
	}
	rate, known := modelPrice(record.PricingModel)
	if !known {
		rate, known = modelPrice(record.Model)
	}
	if !known || (tokens.Input.CacheWriteTokens > 0 && rate.write == 0) {
		return 0, false
	}
	if rate.longAbove > 0 && tokens.Input.TotalTokens > rate.longAbove {
		rate.input *= rate.longInput
		rate.read *= rate.longInput
		rate.write *= rate.longInput
		rate.output *= rate.longOutput
	}
	tier := strings.ToLower(strings.TrimSpace(record.ServiceTier))
	factor, cacheFactor := 1.0, 1.0
	switch tier {
	case "", "auto", "default", "standard":
	case "flex", "batch":
		factor = .5
		// Google cached input retains its standard price for Batch and Flex.
		if rate.vendor != "google" {
			cacheFactor = .5
		}
	case "priority", "fast":
		if rate.vendor != "openai" {
			return 0, false
		}
		factor, cacheFactor = 2, 2
	case "ultrafast":
		if !strings.HasPrefix(record.Model, "gpt-6") {
			return 0, false
		}
		factor, cacheFactor = 6, 6
	default:
		return 0, false
	}
	cost := (float64(tokens.Input.UncachedTokens)*rate.input*factor +
		float64(tokens.Input.CacheReadTokens)*rate.read*cacheFactor +
		float64(tokens.Input.CacheWriteTokens)*rate.write*factor +
		float64(tokens.Output.TotalTokens)*rate.output*factor) / 1_000_000
	return cost, true
}
