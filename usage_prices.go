package main

import "strings"

// USD per million tokens, checked against provider pricing on October 11, 2026.
// Cached reads and cache writes are included in prompt tokens, not added twice.
type usagePrice struct {
	Input, Cached, Write, Write1h, Output                     float64
	Threshold                                                 int64
	LongInput, LongCached, LongWrite, LongWrite1h, LongOutput float64
}

var usagePrices = map[string]usagePrice{
	"gpt-6-astra":       {Input: 10, Cached: 1, Write: 12.5, Output: 50, Threshold: 272000, LongInput: 20, LongCached: 2, LongWrite: 25, LongOutput: 75},
	"gpt-6.1-sol":       {Input: 2, Cached: .1, Write: 2.5, Output: 10, Threshold: 272000, LongInput: 4, LongCached: .2, LongWrite: 5, LongOutput: 15},
	"gpt-6-sol":         {Input: 2, Cached: .2, Write: 2.5, Output: 10, Threshold: 272000, LongInput: 4, LongCached: .4, LongWrite: 5, LongOutput: 15},
	"gpt-6-luna":        {Input: .1, Cached: .01, Write: .125, Output: .5, Threshold: 272000, LongInput: .2, LongCached: .02, LongWrite: .25, LongOutput: .75},
	"gpt-5.6-sol":       {Input: 4, Cached: .4, Write: 5, Output: 20, Threshold: 272000, LongInput: 8, LongCached: .8, LongWrite: 10, LongOutput: 30},
	"gpt-5.6-terra":     {Input: 2, Cached: .2, Write: 2.5, Output: 12, Threshold: 272000, LongInput: 4, LongCached: .4, LongWrite: 5, LongOutput: 18},
	"gpt-5.6-luna":      {Input: .2, Cached: .02, Write: .25, Output: 1.2, Threshold: 272000, LongInput: .4, LongCached: .04, LongWrite: .5, LongOutput: 1.8},
	"gpt-5.5":           {Input: 5, Cached: .5, Write: 5, Output: 30, Threshold: 272000, LongInput: 10, LongCached: 1, LongWrite: 10, LongOutput: 45},
	"gpt-5.5-pro":       {Input: 30, Cached: 30, Write: 30, Output: 180, Threshold: 272000, LongInput: 60, LongCached: 60, LongWrite: 60, LongOutput: 270},
	"gpt-5.4":           {Input: 2.5, Cached: .25, Write: 2.5, Output: 15, Threshold: 272000, LongInput: 5, LongCached: .5, LongWrite: 5, LongOutput: 22.5},
	"gpt-5.4-mini":      {Input: .75, Cached: .075, Write: .75, Output: 4.5},
	"gpt-5.4-nano":      {Input: .2, Cached: .02, Write: .2, Output: 1.25},
	"gpt-5.3-codex":     {Input: 1.75, Cached: .175, Write: 1.75, Output: 14},
	"gpt-5.2":           {Input: 1.75, Cached: .175, Write: 1.75, Output: 14},
	"gpt-5.2-codex":     {Input: 1.75, Cached: .175, Write: 1.75, Output: 14},
	"gpt-5.1":           {Input: 1.25, Cached: .125, Write: 1.25, Output: 10},
	"gpt-5.1-codex":     {Input: 1.25, Cached: .125, Write: 1.25, Output: 10},
	"gpt-5.1-codex-max": {Input: 1.25, Cached: .125, Write: 1.25, Output: 10},
	"gpt-5-codex":       {Input: 1.25, Cached: .125, Write: 1.25, Output: 10},
	"gpt-5":             {Input: 1.25, Cached: .125, Write: 1.25, Output: 10},
	"claude-haiku-5-5":  {Input: .1, Cached: .01, Write: .125, Write1h: .2, Output: .5, Threshold: 100000, LongInput: .5, LongCached: .05, LongWrite: .625, LongWrite1h: 1, LongOutput: 2.5},
	"claude-opus-5-5":   {Input: 4, Cached: .2, Write: 5, Write1h: 8, Output: 20},
	"claude-sonnet-5-5": {Input: 2, Cached: .1, Write: 2.5, Write1h: 4, Output: 10},
	"claude-fable-5-1":  {Input: 10, Cached: .25, Write: 12.5, Write1h: 20, Output: 50},
	"claude-fable-5":    {Input: 10, Cached: 1, Write: 12.5, Write1h: 20, Output: 50},
	"claude-mythos-5-1": {Input: 10, Cached: .25, Write: 12.5, Write1h: 20, Output: 50},
	"claude-mythos-5":   {Input: 10, Cached: 1, Write: 12.5, Write1h: 20, Output: 50},
	"claude-opus-5":     {Input: 5, Cached: .5, Write: 6.25, Write1h: 10, Output: 25},
	"claude-opus-4-8":   {Input: 5, Cached: .5, Write: 6.25, Write1h: 10, Output: 25},
	"claude-opus-4-7":   {Input: 5, Cached: .5, Write: 6.25, Write1h: 10, Output: 25},
	"claude-opus-4-6":   {Input: 5, Cached: .5, Write: 6.25, Write1h: 10, Output: 25},
	"claude-opus-4-5":   {Input: 5, Cached: .5, Write: 6.25, Write1h: 10, Output: 25},
	"claude-sonnet-5":   {Input: 2, Cached: .2, Write: 2.5, Write1h: 4, Output: 10},
	"claude-sonnet-4-6": {Input: 3, Cached: .3, Write: 3.75, Write1h: 6, Output: 15},
	"claude-sonnet-4-5": {Input: 3, Cached: .3, Write: 3.75, Write1h: 6, Output: 15, Threshold: 200000, LongInput: 6, LongCached: .6, LongWrite: 7.5, LongWrite1h: 12, LongOutput: 22.5},
	"claude-haiku-4-5":  {Input: 1, Cached: .1, Write: 1.25, Write1h: 2, Output: 5},
	"claude-haiku-3-5":  {Input: .8, Cached: .08, Write: 1, Write1h: 1.6, Output: 4},
}

func normalizedUsageModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	// A dated model ID uses the same published model rates.
	if n := len(model); n > 9 && model[n-9] == '-' {
		suffix := model[n-8:]
		digits := true
		for _, c := range suffix {
			if c < '0' || c > '9' {
				digits = false
			}
		}
		if digits {
			model = model[:n-9]
		}
	}
	if model == "gpt-5.6" {
		return "gpt-5.6-sol"
	}
	return model
}
func estimateUsageCost(provider, model, tier string, t usageTokens) (float64, bool) {
	p, ok := usagePrices[normalizedUsageModel(model)]
	if !ok {
		return 0, false
	}
	if p.Threshold > 0 && t.Input > p.Threshold {
		p.Input = p.LongInput
		p.Cached = p.LongCached
		p.Write = p.LongWrite
		p.Write1h = p.LongWrite1h
		p.Output = p.LongOutput
	}
	multiplier := 1.0
	if provider == "codex" {
		switch tier {
		case "fast", "priority":
			multiplier = 2
		case "ultrafast":
			multiplier = 6
		case "batch", "flex":
			multiplier = .5
		}
	}
	noncached := max(0, t.Input-t.Cached-t.Write)
	shortWrites := max(0, t.Write-t.Write1h)
	return (float64(noncached)*p.Input + float64(t.Cached)*p.Cached + float64(shortWrites)*p.Write + float64(t.Write1h)*p.Write1h + float64(t.Output)*p.Output) * multiplier / 1e6, true
}
