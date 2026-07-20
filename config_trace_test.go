package maniflex

// 11D.5 — the Trace flag sets are spelled out in two places and did not agree.
//
// ApplyDefaults expanded Trace.Enabled only when Steps, Timings and Aborts were
// all unset, naming three of the five sub-flags; traceConfig named all five to
// decide whether tracing is on at all. Each list was hand-written at its call
// site, so adding a sub-flag to PipelineTrace meant remembering two conditions
// in two files, and forgetting either fails silently — tracing that never turns
// on, or a shorthand that stops expanding.

import "testing"

func TestTraceDefaults_EnabledExpansion(t *testing.T) {
	cases := []struct {
		name string
		in   PipelineTrace
		want PipelineTrace
	}{
		{
			// The shorthand: the standard three, and only those. Bodies and Skips
			// stay off because they are high-volume or expose request data.
			name: "enabled_expands_standard_three",
			in:   PipelineTrace{Enabled: true},
			want: PipelineTrace{Enabled: true, Steps: true, Timings: true, Aborts: true},
		},
		{
			// An explicit standard flag means the caller is choosing precisely, so
			// the shorthand must not widen it back out.
			name: "explicit_standard_flag_suppresses_expansion",
			in:   PipelineTrace{Enabled: true, Steps: true},
			want: PipelineTrace{Enabled: true, Steps: true},
		},
		{
			// Bodies is additive, not a precision choice: it is never produced by
			// the shorthand, so asking for it cannot be read as declining the rest.
			name: "bodies_is_additive_not_a_narrowing",
			in:   PipelineTrace{Enabled: true, Bodies: true},
			want: PipelineTrace{Enabled: true, Steps: true, Timings: true, Aborts: true, Bodies: true},
		},
		{
			name: "skips_is_additive_too",
			in:   PipelineTrace{Enabled: true, Skips: true},
			want: PipelineTrace{Enabled: true, Steps: true, Timings: true, Aborts: true, Skips: true},
		},
		{
			// No shorthand: nothing is inferred.
			name: "sub_flags_alone_are_untouched",
			in:   PipelineTrace{Bodies: true},
			want: PipelineTrace{Bodies: true},
		},
		{
			name: "zero_stays_zero",
			in:   PipelineTrace{},
			want: PipelineTrace{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Trace: tc.in}
			c.ApplyDefaults()
			if c.Trace != tc.want {
				t.Errorf("Trace = %+v, want %+v", c.Trace, tc.want)
			}
		})
	}
}

// TestTraceConfig_ActiveWhenAnyFlagSet: traceConfig gates every hot path, so a
// flag it forgets is a flag that does nothing. Each sub-flag must switch it on
// by itself.
func TestTraceConfig_ActiveWhenAnyFlagSet(t *testing.T) {
	if c := (Config{}); c.traceConfig() != nil {
		t.Error("no flags set: traceConfig must be nil so hot paths skip tracing")
	}
	// Enabled alone is not enough — it is expanded by ApplyDefaults, and a Config
	// that never had defaults applied carries no active flag.
	for _, tc := range []struct {
		name string
		tr   PipelineTrace
	}{
		{"steps", PipelineTrace{Steps: true}},
		{"timings", PipelineTrace{Timings: true}},
		{"aborts", PipelineTrace{Aborts: true}},
		{"bodies", PipelineTrace{Bodies: true}},
		{"skips", PipelineTrace{Skips: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Trace: tc.tr}
			if c.traceConfig() == nil {
				t.Errorf("%s set but traceConfig is nil — the flag would do nothing", tc.name)
			}
		})
	}
}
