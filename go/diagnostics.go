package flashcatgenai

import (
	"encoding/json"
	"runtime/debug"
	"sort"
	"sync"
)

// Diagnostics is exposed by the *SDK returned from Init and by GetDiagnostics. Its JSON fields follow contract §8.
type Diagnostics struct {
	SDK              SDKInfo                 `json:"sdk"`
	Enabled          bool                    `json:"enabled"`
	ServiceName      string                  `json:"service_name"` // Marshals as null when empty.
	Environment      string                  `json:"environment"`
	Endpoint         string                  `json:"endpoint"` // Marshals as null when empty.
	Problems         []string                `json:"problems"`
	Notes            []string                `json:"notes"`
	Provider         ProviderInfo            `json:"provider"`
	Content          map[string]string       `json:"content"`
	Instrumentations []InstrumentationStatus `json:"instrumentations"`
	Export           ExportStats             `json:"export"`
	Semantics        SemanticsStats          `json:"semantics"`
}

// SDKInfo is the sdk field in diagnostics.
type SDKInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Language string `json:"language"`
}

// ProviderInfo is the provider field in diagnostics. Situation is A, B, C, or unsupported.
// ExporterMode is add, reuse, or none. Metrics is add, reuse, none, or disabled.
type ProviderInfo struct {
	Situation    string `json:"situation"`
	ExporterMode string `json:"exporter_mode"`
	Metrics      string `json:"metrics"`
}

// InstrumentationStatus is an entry in the instrumentations field. Go lists only registered instrumentation.
type InstrumentationStatus struct {
	Lib        string `json:"lib"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	LibVersion string `json:"lib_version,omitempty"`
}

// SemanticsStats is the semantics field in diagnostics.
type SemanticsStats struct {
	OrphanInferenceSpans int64 `json:"orphan_inference_spans"`
	NestedInferenceSpans int64 `json:"nested_inference_spans"`
	MaskedSpans          int64 `json:"masked_spans"`
	TruncatedSpans       int64 `json:"truncated_spans"`
}

// MarshalJSON writes nil problems, notes, and instrumentations as empty arrays.
// Empty service_name and endpoint values are written as null.
func (d Diagnostics) MarshalJSON() ([]byte, error) {
	// Match the field order in the contract §8 example.
	out := struct {
		SDK              SDKInfo                 `json:"sdk"`
		Enabled          bool                    `json:"enabled"`
		ServiceName      *string                 `json:"service_name"`
		Environment      string                  `json:"environment"`
		Endpoint         *string                 `json:"endpoint"`
		Problems         []string                `json:"problems"`
		Notes            []string                `json:"notes"`
		Provider         ProviderInfo            `json:"provider"`
		Content          map[string]string       `json:"content"`
		Instrumentations []InstrumentationStatus `json:"instrumentations"`
		Export           ExportStats             `json:"export"`
		Semantics        SemanticsStats          `json:"semantics"`
	}{SDK: d.SDK, Enabled: d.Enabled, Environment: d.Environment, Problems: d.Problems, Notes: d.Notes, Provider: d.Provider,
		Content: d.Content, Instrumentations: d.Instrumentations, Export: d.Export, Semantics: d.Semantics}
	if d.ServiceName != "" {
		out.ServiceName = &d.ServiceName
	}
	if d.Endpoint != "" {
		out.Endpoint = &d.Endpoint
	}
	if out.Problems == nil {
		out.Problems = []string{}
	}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	if out.Instrumentations == nil {
		out.Instrumentations = []InstrumentationStatus{}
	}
	if out.Content == nil {
		out.Content = map[string]string{}
	}
	return json.Marshal(out)
}

func (d Diagnostics) clone() Diagnostics {
	c := d
	c.Problems = append([]string{}, d.Problems...)
	c.Notes = append([]string{}, d.Notes...)
	c.Content = make(map[string]string, len(d.Content))
	for k, v := range d.Content {
		c.Content[k] = v
	}
	return c
}

// ---- Instrumentation registration ----

var registry struct {
	mu    sync.Mutex
	items []InstrumentationStatus
}

// RegisterInstrumentation is called when an instrumentation submodule such as flashcatopenai or flashcateino is first constructed.
// It adds the library to instrumentations in diagnostics with status=installed. lib is the diagnostics library name.
// modulePath is the target library's Go module path, used to look up the version in build information.
// Each lib is recorded once. Application code does not need to call this function.
func RegisterInstrumentation(lib, modulePath string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, it := range registry.items {
		if it.Lib == lib {
			return
		}
	}
	registry.items = append(registry.items, InstrumentationStatus{Lib: lib, Source: "flashcat", Status: "installed", LibVersion: moduleVersion(modulePath)})
}

func registeredInstrumentations() []InstrumentationStatus {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := append([]InstrumentationStatus{}, registry.items...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Lib < out[j].Lib })
	return out
}

func moduleVersion(path string) string {
	if path == "" {
		return ""
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, m := range bi.Deps {
		if m.Path == path {
			if m.Replace != nil && m.Replace.Version != "" {
				return m.Replace.Version
			}
			return m.Version
		}
	}
	return ""
}
