// Copyright 2019 OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sourceprocessor

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/model/pdata"

	"github.com/SumoLogic/sumologic-otel-collector/pkg/processor/sourceprocessor/observability"
)

type sourceKeys struct {
	annotationPrefix   string
	podKey             string
	podNameKey         string
	podTemplateHashKey string
}

// dockerLog represents log from k8s using docker log driver send by FluentBit
type dockerLog struct {
	Stream string
	Time   string
	Log    string
}

type sourceProcessor struct {
	collector            string
	sourceCategoryFiller sourceCategoryFiller
	sourceNameFiller     attributeFiller
	sourceHostFiller     attributeFiller

	exclude map[string]*regexp.Regexp
	keys    sourceKeys
}

const (
	alphanums = "bcdfghjklmnpqrstvwxz2456789"

	sourceHostSpecialAnnotation         = "sumologic.com/sourceHost"
	sourceNameSpecialAnnotation         = "sumologic.com/sourceName"
	sourceCategorySpecialAnnotation     = "sumologic.com/sourceCategory"
	sourceCategoryPrefixAnnotation      = "sumologic.com/sourceCategoryPrefix"
	sourceCategoryReplaceDashAnnotation = "sumologic.com/sourceCategoryReplaceDash"

	includeAnnotation = "sumologic.com/include"
	excludeAnnotation = "sumologic.com/exclude"

	collectorKey      = "_collector"
	sourceCategoryKey = "_sourceCategory"
	sourceHostKey     = "_sourceHost"
	sourceNameKey     = "_sourceName"
)

func compileRegex(regex string) *regexp.Regexp {
	if regex == "" {
		return nil
	}

	re, err := regexp.Compile(regex)
	if err != nil {
		log.Fatalf("Cannot compile regular expression: %s Error: %v\n", regex, err)
	}

	return re
}

func newSourceProcessor(cfg *Config) *sourceProcessor {
	keys := sourceKeys{
		annotationPrefix:   cfg.AnnotationPrefix,
		podKey:             cfg.PodKey,
		podNameKey:         cfg.PodNameKey,
		podTemplateHashKey: cfg.PodTemplateHashKey,
	}

	exclude := make(map[string]*regexp.Regexp)
	for field, regexStr := range cfg.Exclude {
		if r := compileRegex(regexStr); r != nil {
			exclude[field] = r
		}
	}

	return &sourceProcessor{
		collector:            cfg.Collector,
		keys:                 keys,
		sourceHostFiller:     createSourceHostFiller(cfg),
		sourceCategoryFiller: newSourceCategoryFiller(cfg),
		sourceNameFiller:     createSourceNameFiller(cfg),
		exclude:              exclude,
	}
}

func (sp *sourceProcessor) fillOtherMeta(atts pdata.AttributeMap) {
	if sp.collector != "" {
		atts.UpsertString(collectorKey, sp.collector)
	}
}

func (sp *sourceProcessor) isFilteredOut(atts pdata.AttributeMap) bool {
	// TODO: This is quite inefficient when done for each package (ore even more so, span) separately.
	// It should be moved to K8S Meta Processor and done once per new pod/changed pod

	if value, found := atts.Get(sp.annotationAttribute(excludeAnnotation)); found {
		if value.Type() == pdata.AttributeValueTypeString && value.StringVal() == "true" {
			return true
		} else if value.Type() == pdata.AttributeValueTypeBool && value.BoolVal() {
			return true
		}
	}

	if value, found := atts.Get(sp.annotationAttribute(includeAnnotation)); found {
		if value.Type() == pdata.AttributeValueTypeString && value.StringVal() == "true" {
			return false
		} else if value.Type() == pdata.AttributeValueTypeBool && value.BoolVal() {
			return false
		}
	}

	// Check fields by matching them against field exclusion regexes
	for field, r := range sp.exclude {
		_, ok := matchFieldByRegex(atts, field, r)
		if ok {
			return true
		}
	}

	return false
}

func (sp *sourceProcessor) annotationAttribute(annotationKey string) string {
	return sp.keys.annotationPrefix + annotationKey
}

// ProcessTraces processes traces
func (sp *sourceProcessor) ProcessTraces(ctx context.Context, td pdata.Traces) (pdata.Traces, error) {
	rss := td.ResourceSpans()

	for i := 0; i < rss.Len(); i++ {
		observability.RecordResourceSpansProcessed()

		rs := rss.At(i)
		res := sp.processResource(rs.Resource())
		atts := res.Attributes()

		ilss := rs.InstrumentationLibrarySpans()
		totalSpans := 0
		for j := 0; j < ilss.Len(); j++ {
			ils := ilss.At(j)
			totalSpans += ils.Spans().Len()
		}

		if sp.isFilteredOut(atts) {
			rs.InstrumentationLibrarySpans().RemoveIf(func(pdata.InstrumentationLibrarySpans) bool { return true })
			observability.RecordFilteredOutN(totalSpans)
		} else {
			observability.RecordFilteredInN(totalSpans)
		}
	}

	return td, nil
}

// ProcessMetrics processes metrics
func (sp *sourceProcessor) ProcessMetrics(ctx context.Context, md pdata.Metrics) (pdata.Metrics, error) {
	rss := md.ResourceMetrics()

	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		res := sp.processResource(rs.Resource())
		atts := res.Attributes()

		if sp.isFilteredOut(atts) {
			rs.InstrumentationLibraryMetrics().RemoveIf(func(pdata.InstrumentationLibraryMetrics) bool { return true })
		}
	}

	return md, nil
}

// ProcessLogs processes logs
func (sp *sourceProcessor) ProcessLogs(ctx context.Context, md pdata.Logs) (pdata.Logs, error) {
	rss := md.ResourceLogs()

	var dockerLog dockerLog

	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		res := sp.processResource(rs.Resource())
		atts := res.Attributes()

		if sp.isFilteredOut(atts) {
			rs.InstrumentationLibraryLogs().RemoveIf(func(pdata.InstrumentationLibraryLogs) bool { return true })
		}

		// Due to fluent-bit configuration for sumologic kubernetes collection,
		// logs from kubernetes with docker log driver are send as json with
		// `log`, `stream` and `time` keys.
		// We would like to extract `time` and `stream` to record level attributes
		// and treat `log` as body of the log
		//
		// Related issue: https://github.com/SumoLogic/sumologic-kubernetes-collection/issues/1758
		// ToDo: remove this functionality when the issue is resolved

		ills := rs.InstrumentationLibraryLogs()
		for j := 0; j < ills.Len(); j++ {
			ill := ills.At(j)
			logs := ill.LogRecords()
			for k := 0; k < logs.Len(); k++ {
				log := logs.At(k)
				if log.Body().Type() == pdata.AttributeValueTypeString {
					err := json.Unmarshal([]byte(log.Body().StringVal()), &dockerLog)

					// If there was any parsing error or any of the expected key have no value
					// skip extraction and leave log unchanged
					if err != nil || dockerLog.Stream == "" || dockerLog.Time == "" || dockerLog.Log == "" {
						continue
					}

					// Extract `stream` and `time` as record level attributes
					log.Attributes().UpsertString("stream", dockerLog.Stream)
					log.Attributes().UpsertString("time", dockerLog.Time)

					// Set log body to `log` content
					log.Body().SetStringVal(strings.TrimSpace(dockerLog.Log))
				}
			}
		}
	}

	return md, nil
}

// processResource performs multiple actions on resource:
//   - enrich pod name, so it can be used in templates
//   - fills source attributes based on config or annotations
//   - set metadata (collector name)
func (sp *sourceProcessor) processResource(res pdata.Resource) pdata.Resource {
	atts := res.Attributes()

	sp.enrichPodName(&atts)
	sp.fillOtherMeta(atts)

	sp.sourceHostFiller.fillResourceOrUseAnnotation(&atts,
		sp.annotationAttribute(sourceHostSpecialAnnotation),
	)
	sp.sourceCategoryFiller.fill(&atts)
	sp.sourceNameFiller.fillResourceOrUseAnnotation(&atts,
		sp.annotationAttribute(sourceNameSpecialAnnotation),
	)

	return res
}

// Start is invoked during service startup.
func (*sourceProcessor) Start(_context context.Context, _host component.Host) error {
	return nil
}

// Shutdown is invoked during service shutdown.
func (*sourceProcessor) Shutdown(_context context.Context) error {
	return nil
}

// Convert the pod_template_hash to an alphanumeric string using the same logic Kubernetes
// uses at https://github.com/kubernetes/apimachinery/blob/18a5ff3097b4b189511742e39151a153ee16988b/pkg/util/rand/rand.go#L119
func SafeEncodeString(s string) string {
	r := make([]byte, len(s))
	for i, b := range []rune(s) {
		r[i] = alphanums[(int(b) % len(alphanums))]
	}
	return string(r)
}

func (sp *sourceProcessor) enrichPodName(atts *pdata.AttributeMap) {
	// This replicates sanitize_pod_name function
	// Strip out dynamic bits from pod name.
	// NOTE: Kubernetes deployments append a template hash.
	// At the moment this can be in 3 different forms:
	//   1) pre-1.8: numeric in pod_template_hash and pod_parts[-2]
	//   2) 1.8-1.11: numeric in pod_template_hash, hash in pod_parts[-2]
	//   3) post-1.11: hash in pod_template_hash and pod_parts[-2]

	if atts == nil {
		return
	}
	pod, found := atts.Get(sp.keys.podKey)
	if !found {
		return
	}

	podParts := strings.Split(pod.StringVal(), "-")
	if len(podParts) < 2 {
		// This is unexpected, fallback
		return
	}

	podTemplateHashAttr, found := atts.Get(sp.keys.podTemplateHashKey)

	if found && len(podParts) > 2 {
		podTemplateHash := podTemplateHashAttr.StringVal()
		if podTemplateHash == podParts[len(podParts)-2] || SafeEncodeString(podTemplateHash) == podParts[len(podParts)-2] {
			atts.UpsertString(sp.keys.podNameKey, strings.Join(podParts[:len(podParts)-2], "-"))
			return
		}
	}
	atts.UpsertString(sp.keys.podNameKey, strings.Join(podParts[:len(podParts)-1], "-"))
}

// matchFieldByRegex searches the provided attribute map for a particular field
// and matches is with the provided regex.
// It returns the string value of found elements and a boolean flag whether the
// value matched the provided regex.
func matchFieldByRegex(atts pdata.AttributeMap, field string, r *regexp.Regexp) (string, bool) {
	att, ok := atts.Get(field)
	if !ok {
		return "", false
	}

	if att.Type() != pdata.AttributeValueTypeString {
		return "", false
	}

	v := att.StringVal()
	return v, r.MatchString(v)
}
