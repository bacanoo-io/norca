/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';

// Admission policies (kubernetes-features-spec.md, item 2).

export function matchText(match) {
  const parts = [];
  const ns = match?.Namespaces || [];
  parts.push(ns.length ? `namespaces ${ns.join(', ')}` : 'every namespace');
  const res = match?.Resources || [];
  parts.push(res.length ? res.join(', ') : 'jobs');
  if (match?.VariablePaths?.length)
    parts.push(`paths ${match.VariablePaths.join(', ')}`);
  return parts.join('; ');
}

// The JSON form of an admission policy (the body of PUT
// /v1/admission-policy/:name), checked in the unified editor. The server
// compiles the CEL expressions when the policy is written.
const strings = { type: ['array', 'null'], items: { type: 'string' } };

export const ADMISSION_SCHEMA = {
  type: 'object',
  required: ['Name', 'Rules'],
  additionalProperties: false,
  properties: {
    Name: {
      type: 'string',
      pattern: '^[a-zA-Z0-9-_]{1,128}$',
      patternMessage: 'must be 1 to 128 letters, digits, - or _',
    },
    Description: { type: 'string', maxLength: 256 },
    Action: { type: 'string', enum: ['deny', 'warn', ''] },
    Match: {
      type: ['object', 'null'],
      additionalProperties: false,
      properties: {
        Namespaces: strings,
        Resources: {
          type: ['array', 'null'],
          items: { type: 'string', enum: ['job', 'variable', 'release'] },
        },
        VariablePaths: strings,
      },
    },
    SchemaFrom: { type: 'string', enum: ['', 'crd-schema'] },
    Rules: {
      type: 'array',
      maxItems: 64,
      items: {
        type: 'object',
        required: ['Expression'],
        additionalProperties: false,
        properties: {
          Expression: { type: 'string', minLength: 1, maxLength: 8192 },
          Message: { type: 'string' },
        },
      },
    },
    TrustedKeys: strings,
  },
};

export const NEW_ADMISSION_POLICY = {
  Name: 'require-owner',
  Description: 'Every job names its owning team',
  Action: 'warn',
  Match: { Namespaces: ['*'], Resources: ['job'] },
  Rules: [
    {
      Expression: "has(job.Meta) && 'owner' in job.Meta",
      Message: 'Set meta.owner to the team that owns the job.',
    },
  ],
};

// The editable JSON of a policy: the stored fields without the indexes.
export function admissionPolicyText(policy) {
  if (!policy) return JSON.stringify(NEW_ADMISSION_POLICY, null, 2);
  // eslint-disable-next-line no-unused-vars
  const { CreateIndex, ModifyIndex, ...rest } = policy;
  return JSON.stringify(rest, null, 2);
}

// What the CEL expressions of a policy can use, for the editor's reference.
export const CEL_REFERENCE = [
  {
    name: 'job',
    text: 'The submitted job in the API form (job.ID, job.Meta, job.TaskGroups, ...); for job resources.',
  },
  {
    name: 'request',
    text: 'The write: request.resource, request.operation, request.namespace and request.user.',
  },
  {
    name: 'variable / oldVariable',
    text: 'The written Variable (variable.path, variable.namespace, variable.items) and the stored one, or null; for variable resources.',
  },
  {
    name: 'release',
    text: 'The release: release.name and release.pack (name, version, registry, digest, signer, signed); for release resources.',
  },
];

export const CEL_EXAMPLES = [
  'job.TaskGroups.all(tg, tg.Count <= 10)',
  "!job.TaskGroups.exists(tg, tg.Tasks.exists(t, t.Driver == 'raw_exec'))",
  "request.namespace != 'prod' || has(job.Meta) && 'owner' in job.Meta",
];

export function denialItem(event) {
  const d = event.Payload || {};
  const time = moment();
  return {
    icon: 'shield-check',
    color: 'red',
    title: `${d.Operation === 'plan' ? 'Plan' : 'Submission'} of ${
      d.JobID || d.Path
    } denied`,
    text: [
      `${d.Controller === 'webhook' ? 'webhook' : 'policy'} ${d.Name}`,
      d.Message,
      d.User?.Name && `by ${d.User.Name}`,
    ]
      .filter(Boolean)
      .join(': '),
    time: time.format('HH:mm:ss'),
    name: d.Name,
  };
}
