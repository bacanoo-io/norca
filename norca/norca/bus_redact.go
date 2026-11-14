// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"regexp"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The Job events on the system bus carry the job without the parts that
// may hold a secret written into the job: the values of env and meta, the
// embedded templates, the task driver configurations (e.g. the auth of a
// docker image), the credentials of the artifacts, the headers and bodies
// of the service checks, and the payload of a dispatched job. What a
// consumer of the events needs stays: IDs, version, status, groups and
// their counts, task names and drivers, the keys of env and meta. The
// event stream API and the state keep the whole job; a consumer reads it
// there with read-job.

// busEventJob returns the job an event is about: the key of a Job event,
// the first filter key (the job ID) of the events of evaluations,
// allocations, deployments and service registrations; empty for the other
// topics, whose filter keys are something else.
func busEventJob(ev *structs.Event) string {
	switch ev.Topic {
	case structs.TopicJob:
		return ev.Key
	case structs.TopicEvaluation, structs.TopicAllocation, structs.TopicDeployment, structs.TopicService:
		if len(ev.FilterKeys) > 0 {
			return ev.FilterKeys[0]
		}
	}
	return ""
}

// busRedacted replaces a value left out of a bus event.
const busRedacted = "<redacted>"

// busRedactEvent returns a copy of an event with its secrets left out, and
// whether it left out anything; events of other topics are returned as
// they are.
func busRedactEvent(ev *structs.Event) (structs.Event, bool) {
	if ev.Topic != structs.TopicJob {
		return *ev, false
	}
	payload, ok := ev.Payload.(*structs.JobEvent)
	if !ok || payload == nil || payload.Job == nil {
		return *ev, false
	}
	redacted := *ev
	redacted.Payload = &structs.JobEvent{Job: busRedactJob(payload.Job)}
	return redacted, true
}

// busRedactJob returns a copy of a job without the parts that may hold a
// secret.
func busRedactJob(job *structs.Job) *structs.Job {
	j := job.Copy()
	j.Payload = nil
	j.Meta = redactValues(j.Meta)
	for _, tg := range j.TaskGroups {
		tg.Meta = redactValues(tg.Meta)
		redactChecks(tg.Services)
		for _, task := range tg.Tasks {
			task.Env = redactValues(task.Env)
			task.Meta = redactValues(task.Meta)
			if task.Config != nil {
				task.Config = map[string]interface{}{}
			}
			for _, tmpl := range task.Templates {
				if tmpl.EmbeddedTmpl != "" {
					tmpl.EmbeddedTmpl = busRedacted
				}
			}
			for _, a := range task.Artifacts {
				a.GetterSource = redactURL(a.GetterSource)
				a.GetterOptions = redactValues(a.GetterOptions)
				a.GetterHeaders = redactValues(a.GetterHeaders)
			}
			redactChecks(task.Services)
		}
	}
	return j
}

// redactValues keeps the keys of a map and replaces its values.
func redactValues(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = busRedacted
	}
	return out
}

// redactChecks leaves out the headers and bodies of the service checks.
func redactChecks(services []*structs.Service) {
	for _, s := range services {
		for _, c := range s.Checks {
			for k := range c.Header {
				c.Header[k] = []string{busRedacted}
			}
			if c.Body != "" {
				c.Body = busRedacted
			}
		}
	}
}

var (
	urlUserInfo = regexp.MustCompile(`//[^/@?#]+@`)
	urlQuery    = regexp.MustCompile(`\?.*$`)
)

// redactURL leaves out the user information and the query of an artifact
// source ("https://user:pass@host/x?token=..."), where go-getter takes
// credentials from.
func redactURL(source string) string {
	source = urlUserInfo.ReplaceAllString(source, "//"+busRedacted+"@")
	return urlQuery.ReplaceAllString(source, "?"+busRedacted)
}
