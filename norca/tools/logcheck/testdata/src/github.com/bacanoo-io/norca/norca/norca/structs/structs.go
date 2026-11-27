// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package structs is a stub of the server's structs for the tests.
package structs

type Allocation struct{ ID, Name string }

type Job struct{ ID, Namespace string }

func (j *Job) NamespacedID() NamespacedID { return NamespacedID{ID: j.ID, Namespace: j.Namespace} }

type NamespacedID struct{ ID, Namespace string }

type ACLToken struct{ AccessorID, SecretID string }

type Plan struct{ EvalID string }
