// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/pointer"
	"golang.org/x/crypto/blake2b"
)

const (
	// QuotaEnforceSchedule is the default enforcement mode of a quota: jobs
	// are always accepted and the scheduler places allocations only up to the
	// quota limit. The remaining placements are blocked until quota is freed.
	QuotaEnforceSchedule = "schedule"

	// QuotaEnforceSubmit additionally rejects a job at submission if its
	// total demand would by itself exceed the quota limit.
	QuotaEnforceSubmit = "submit"

	// Quota dimensions, used when reporting which limit was hit.
	QuotaDimensionCPU         = "cpu"
	QuotaDimensionMemory      = "memory"
	QuotaDimensionMemoryMax   = "memory_max"
	QuotaDimensionAllocations = "allocations"
	QuotaDimensionVariables   = "variables"

	// maxQuotaDescriptionLength is the maximum length of a quota description.
	maxQuotaDescriptionLength = 256
)

const (
	// TopicQuota is the event stream topic of quota specifications and
	// quota usage changes.
	TopicQuota Topic = "Quota"

	TypeQuotaSpecUpserted = "QuotaSpecUpserted"
	TypeQuotaSpecDeleted  = "QuotaSpecDeleted"

	// TypeQuotaExhausted is emitted when a quota usage reaches its limit in
	// at least one dimension that was not exhausted before.
	TypeQuotaExhausted = "QuotaExhausted"

	// TypeQuotaFreed is emitted when a previously exhausted dimension of a
	// quota usage drops below its limit again.
	TypeQuotaFreed = "QuotaFreed"
)

var (
	// validQuotaName is the rule used to validate a quota name.
	validQuotaName = regexp.MustCompile("^[a-zA-Z0-9-_]{1,128}$")

	// QuotaCountMemoryMax controls the provisional answer to the open design
	// question of whether memory_max counts toward the memory limit. When
	// false (the default) the "memory" limit is compared with the reserved
	// memory of the allocations only; memory_max usage is still tracked and
	// is enforced against a separately configured memory_max limit. When
	// true, the "memory" limit is compared with the memory_max usage.
	QuotaCountMemoryMax = false
)

// QuotaSpec specifies the allowed resource usage of the namespaces that
// reference it, per region. It is the server side counterpart of
// api.QuotaSpec, extended with the fork's task level defaults and maximums
// (the equivalent of a Kubernetes LimitRange) and the enforcement mode.
type QuotaSpec struct {
	// Name is the name of the quota specification. It must be unique.
	Name string

	// Description is an optional description of the quota.
	Description string

	// Limits is the set of per-region limits.
	Limits []*QuotaLimit

	// TaskDefaults are the resource values filled in at job submission for
	// tasks that do not set them. Only CPU, MemoryMB and MemoryMaxMB are
	// supported.
	TaskDefaults *Resources

	// TaskMax are the maximum resource values a single task may request.
	// Only CPU, Cores, MemoryMB and MemoryMaxMB are supported. A zero value
	// means no maximum.
	TaskMax *Resources

	// Enforce is the enforcement mode, either QuotaEnforceSchedule (default)
	// or QuotaEnforceSubmit.
	Enforce string

	// Hash is the hash of the specification and is used to efficiently diff
	// when quotas are replicated across regions.
	Hash []byte

	// Raft indexes.
	CreateIndex uint64
	ModifyIndex uint64
}

// QuotaLimit describes the resource limit in a particular region.
type QuotaLimit struct {
	// Region is the region in which this limit applies.
	Region string

	// RegionLimit is the limit on the sum of the resources of all
	// non-terminal allocations in the namespaces referencing the quota. Only
	// CPU (MHz, reserved cores are converted to MHz), MemoryMB and
	// MemoryMaxMB are supported. A value of zero is treated as unlimited and a
	// negative value as fully disallowed.
	RegionLimit *Resources

	// VariablesLimit is the maximum total size of all variables in MiB. A
	// nil or zero value is treated as unlimited and a negative value as fully
	// disallowed.
	VariablesLimit *int

	// Allocations is the maximum number of non-terminal allocations. A nil or
	// zero value is treated as unlimited and a negative value as fully
	// disallowed.
	Allocations *int

	// Hash is the hash of the limit. Quota usages are keyed by the base64
	// encoding of the hash of the limit they belong to.
	Hash []byte
}

// QuotaUsage is the resource usage of a quota in the local region.
type QuotaUsage struct {
	// Name is the name of the quota specification.
	Name string

	// Used is the usage per limit of the quota, keyed by the base64 encoded
	// hash of the limit. Only limits of the local region are tracked. The
	// resource fields of each entry hold the used amount instead of the
	// limit.
	Used map[string]*QuotaLimit

	// Raft indexes.
	CreateIndex uint64
	ModifyIndex uint64
}

// QuotaResources is a flat vector of the resource dimensions a quota limits
// on allocations.
type QuotaResources struct {
	// CPU is in MHz and includes reserved cores converted to MHz.
	CPU         int64
	MemoryMB    int64
	MemoryMaxMB int64
	Allocations int64
}

// QuotaRemaining describes how much of a quota limit is still available in
// the local region. A nil field means that dimension is unlimited. A value
// can be zero or negative if the quota is exhausted or over-committed (for
// example after the limit was lowered).
type QuotaRemaining struct {
	Quota       string
	Region      string
	CPU         *int64
	MemoryMB    *int64
	MemoryMaxMB *int64
	Allocations *int64
}

// ValidateQuotaName returns an error if a quota name is invalid.
func ValidateQuotaName(name string) error {
	if !validQuotaName.MatchString(name) {
		return fmt.Errorf("invalid name %q, must match regex %s", name, validQuotaName)
	}
	return nil
}

// GetID implements the IDGetter interface required for pagination.
func (q *QuotaSpec) GetID() string {
	return q.Name
}

// GetCreateIndex implements the CreateIndexGetter interface required for
// pagination.
func (q *QuotaSpec) GetCreateIndex() uint64 {
	return q.CreateIndex
}

// Canonicalize sets the defaults of the quota specification.
func (q *QuotaSpec) Canonicalize() {
	if q == nil {
		return
	}
	if q.Enforce == "" {
		q.Enforce = QuotaEnforceSchedule
	}
	for _, l := range q.Limits {
		if l != nil && l.RegionLimit == nil {
			l.RegionLimit = &Resources{}
		}
	}
}

// Validate returns an error if the quota specification is invalid.
func (q *QuotaSpec) Validate() error {
	if q == nil {
		return fmt.Errorf("missing quota specification")
	}

	var mErr *multierror.Error
	if err := ValidateQuotaName(q.Name); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	if len(q.Description) > maxQuotaDescriptionLength {
		mErr = multierror.Append(mErr, fmt.Errorf("description longer than %d", maxQuotaDescriptionLength))
	}

	switch q.Enforce {
	case "", QuotaEnforceSchedule, QuotaEnforceSubmit:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("invalid enforce mode %q, must be %q or %q",
			q.Enforce, QuotaEnforceSchedule, QuotaEnforceSubmit))
	}

	regions := make(map[string]struct{}, len(q.Limits))
	for i, l := range q.Limits {
		if l == nil {
			mErr = multierror.Append(mErr, fmt.Errorf("limit %d is empty", i))
			continue
		}
		if err := l.Validate(); err != nil {
			mErr = multierror.Append(mErr, multierror.Prefix(err, fmt.Sprintf("limit %d:", i)))
		}
		if _, ok := regions[l.Region]; ok {
			mErr = multierror.Append(mErr, fmt.Errorf("limit %d: duplicate limit for region %q", i, l.Region))
		}
		regions[l.Region] = struct{}{}
	}

	if d := q.TaskDefaults; d != nil {
		if d.CPU < 0 || d.MemoryMB < 0 || d.MemoryMaxMB < 0 {
			mErr = multierror.Append(mErr, fmt.Errorf("task_defaults: values must not be negative"))
		}
		if d.Cores != 0 {
			mErr = multierror.Append(mErr, fmt.Errorf("task_defaults: cores is not supported"))
		}
		if err := quotaUnsupportedResources(d); err != nil {
			mErr = multierror.Append(mErr, multierror.Prefix(err, "task_defaults:"))
		}
		if d.MemoryMaxMB != 0 && d.MemoryMB != 0 && d.MemoryMaxMB < d.MemoryMB {
			mErr = multierror.Append(mErr, fmt.Errorf("task_defaults: memory_max must not be lower than memory"))
		}
	}

	if m := q.TaskMax; m != nil {
		if m.CPU < 0 || m.Cores < 0 || m.MemoryMB < 0 || m.MemoryMaxMB < 0 {
			mErr = multierror.Append(mErr, fmt.Errorf("task_max: values must not be negative"))
		}
		if err := quotaUnsupportedResources(m); err != nil {
			mErr = multierror.Append(mErr, multierror.Prefix(err, "task_max:"))
		}
		if d := q.TaskDefaults; d != nil {
			if m.CPU > 0 && d.CPU > m.CPU {
				mErr = multierror.Append(mErr, fmt.Errorf("task_defaults: cpu exceeds task_max"))
			}
			if m.MemoryMB > 0 && d.MemoryMB > m.MemoryMB {
				mErr = multierror.Append(mErr, fmt.Errorf("task_defaults: memory exceeds task_max"))
			}
			if m.MemoryMaxMB > 0 && d.MemoryMaxMB > m.MemoryMaxMB {
				mErr = multierror.Append(mErr, fmt.Errorf("task_defaults: memory_max exceeds task_max"))
			}
		}
	}

	return mErr.ErrorOrNil()
}

// quotaUnsupportedResources returns an error if resource fields that quotas do
// not support are set. Device limits are intentionally not supported in the
// first version.
func quotaUnsupportedResources(r *Resources) error {
	var mErr *multierror.Error
	if r.DiskMB != 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("disk is not supported"))
	}
	if r.IOPS != 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("iops is not supported"))
	}
	if len(r.Networks) != 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("network resources are not supported"))
	}
	if len(r.Devices) != 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("device resources are not supported"))
	}
	return mErr.ErrorOrNil()
}

// Validate returns an error if the quota limit is invalid.
func (l *QuotaLimit) Validate() error {
	var mErr *multierror.Error
	if l.Region == "" {
		mErr = multierror.Append(mErr, fmt.Errorf("missing region"))
	}
	if r := l.RegionLimit; r != nil {
		if r.Cores != 0 {
			mErr = multierror.Append(mErr, fmt.Errorf(
				"region_limit: cores is not supported, reserved cores count toward the cpu limit"))
		}
		if err := quotaUnsupportedResources(r); err != nil {
			mErr = multierror.Append(mErr, multierror.Prefix(err, "region_limit:"))
		}
	}
	return mErr.ErrorOrNil()
}

// SetHash computes and sets the hash of the quota specification and of each
// of its limits.
func (q *QuotaSpec) SetHash() []byte {
	hash, err := blake2b.New256(nil)
	if err != nil {
		panic(err)
	}

	_, _ = hash.Write([]byte(q.Name))
	_, _ = hash.Write([]byte(q.Description))
	_, _ = hash.Write([]byte(q.Enforce))
	for _, l := range q.Limits {
		if l == nil {
			continue
		}
		_, _ = hash.Write(l.SetHash())
	}
	hashQuotaResources(hash, "task_defaults", q.TaskDefaults)
	hashQuotaResources(hash, "task_max", q.TaskMax)

	q.Hash = hash.Sum(nil)
	return q.Hash
}

// SetHash computes and sets the hash of the quota limit.
func (l *QuotaLimit) SetHash() []byte {
	hash, err := blake2b.New256(nil)
	if err != nil {
		panic(err)
	}

	_, _ = hash.Write([]byte(l.Region))
	hashQuotaResources(hash, "region_limit", l.RegionLimit)
	hashQuotaIntPtr(hash, "variables_limit", l.VariablesLimit)
	hashQuotaIntPtr(hash, "allocations", l.Allocations)

	l.Hash = hash.Sum(nil)
	return l.Hash
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func hashQuotaResources(hash hashWriter, name string, r *Resources) {
	_, _ = hash.Write([]byte(name))
	if r == nil {
		return
	}
	for _, v := range []int{r.CPU, r.Cores, r.MemoryMB, r.MemoryMaxMB} {
		_ = binary.Write(hash, binary.BigEndian, int64(v))
	}
}

func hashQuotaIntPtr(hash hashWriter, name string, v *int) {
	_, _ = hash.Write([]byte(name))
	if v != nil {
		_ = binary.Write(hash, binary.BigEndian, int64(*v))
	}
}

// HashKey returns the key under which the usage of this limit is tracked in
// a QuotaUsage.
func (l *QuotaLimit) HashKey() string {
	return base64.StdEncoding.EncodeToString(l.Hash)
}

// Copy returns a deep copy of the quota specification.
func (q *QuotaSpec) Copy() *QuotaSpec {
	if q == nil {
		return nil
	}
	nq := new(QuotaSpec)
	*nq = *q
	nq.Hash = copyBytes(q.Hash)
	if q.Limits != nil {
		nq.Limits = make([]*QuotaLimit, len(q.Limits))
		for i, l := range q.Limits {
			nq.Limits[i] = l.Copy()
		}
	}
	nq.TaskDefaults = q.TaskDefaults.Copy()
	nq.TaskMax = q.TaskMax.Copy()
	return nq
}

// Copy returns a deep copy of the quota limit.
func (l *QuotaLimit) Copy() *QuotaLimit {
	if l == nil {
		return nil
	}
	nl := new(QuotaLimit)
	*nl = *l
	nl.RegionLimit = l.RegionLimit.Copy()
	if l.VariablesLimit != nil {
		nl.VariablesLimit = pointer.Of(*l.VariablesLimit)
	}
	if l.Allocations != nil {
		nl.Allocations = pointer.Of(*l.Allocations)
	}
	nl.Hash = copyBytes(l.Hash)
	return nl
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	nb := make([]byte, len(b))
	copy(nb, b)
	return nb
}

// LimitForRegion returns the limit of the quota that applies to the given
// region or nil if the quota doesn't limit the region.
func (q *QuotaSpec) LimitForRegion(region string) *QuotaLimit {
	if q == nil {
		return nil
	}
	for _, l := range q.Limits {
		if l != nil && l.Region == region {
			return l
		}
	}
	return nil
}

// Resources returns the allocation resource limits as a QuotaResources
// vector. A nil limit is returned as zero, meaning unlimited.
func (l *QuotaLimit) Resources() QuotaResources {
	var r QuotaResources
	if l == nil {
		return r
	}
	if l.RegionLimit != nil {
		r.CPU = int64(l.RegionLimit.CPU)
		r.MemoryMB = int64(l.RegionLimit.MemoryMB)
		r.MemoryMaxMB = int64(l.RegionLimit.MemoryMaxMB)
	}
	if l.Allocations != nil {
		r.Allocations = int64(*l.Allocations)
	}
	return r
}

// quotaLimitExceeded reports whether value violates limit. A limit of zero
// is unlimited and a negative limit disallows any usage.
func quotaLimitExceeded(limit, value int64) bool {
	switch {
	case limit == 0:
		return false
	case limit < 0:
		return value > 0
	default:
		return value > limit
	}
}

// quotaLimitExhausted reports whether there is no capacity left under the
// limit. A limit of zero is unlimited and is never exhausted.
func quotaLimitExhausted(limit, value int64) bool {
	switch {
	case limit == 0:
		return false
	case limit < 0:
		return true
	default:
		return value >= limit
	}
}

// quotaMemoryUsage returns the memory value compared with the "memory" limit.
func quotaMemoryUsage(r QuotaResources) int64 {
	if QuotaCountMemoryMax {
		return r.MemoryMaxMB
	}
	return r.MemoryMB
}

// quotaDimensions returns the dimensions in the canonical order, with the
// limit and used value of each.
func quotaDimensions(limit, used QuotaResources) []struct {
	name        string
	limit, used int64
} {
	return []struct {
		name        string
		limit, used int64
	}{
		{QuotaDimensionCPU, limit.CPU, used.CPU},
		{QuotaDimensionMemory, limit.MemoryMB, quotaMemoryUsage(used)},
		{QuotaDimensionMemoryMax, limit.MemoryMaxMB, used.MemoryMaxMB},
		{QuotaDimensionAllocations, limit.Allocations, used.Allocations},
	}
}

// ExceededDimensions returns the dimensions in which used exceeds the limit.
func (l *QuotaLimit) ExceededDimensions(used QuotaResources) []string {
	var out []string
	for _, d := range quotaDimensions(l.Resources(), used) {
		if quotaLimitExceeded(d.limit, d.used) {
			out = append(out, d.name)
		}
	}
	return out
}

// ExhaustedDimensions returns the dimensions in which no capacity is left.
func (l *QuotaLimit) ExhaustedDimensions(used QuotaResources) []string {
	var out []string
	for _, d := range quotaDimensions(l.Resources(), used) {
		if quotaLimitExhausted(d.limit, d.used) {
			out = append(out, d.name)
		}
	}
	return out
}

// CheckAdditional returns a human readable description of every dimension in
// which adding delta to used would exceed the limit. Dimensions that delta
// does not increase are never reported, so a plan that keeps or reduces the
// usage of an over-committed quota is still allowed.
func (l *QuotaLimit) CheckAdditional(used, delta QuotaResources) []string {
	total := used.Add(delta)
	limit := l.Resources()
	usedDims := quotaDimensions(limit, used)
	var out []string
	for i, d := range quotaDimensions(limit, total) {
		if d.used <= usedDims[i].used {
			continue
		}
		if quotaLimitExceeded(d.limit, d.used) {
			out = append(out, fmt.Sprintf("%s exhausted (%d > %d)", d.name, d.used, max(d.limit, 0)))
		}
	}
	return out
}

// Remaining returns the remaining capacity of the limit given the usage.
func (l *QuotaLimit) Remaining(used QuotaResources) *QuotaRemaining {
	rem := &QuotaRemaining{Region: l.Region}
	remaining := func(limit, used int64) *int64 {
		switch {
		case limit == 0:
			return nil
		case limit < 0:
			return pointer.Of(int64(0))
		default:
			return pointer.Of(limit - used)
		}
	}
	limit := l.Resources()
	rem.CPU = remaining(limit.CPU, used.CPU)
	rem.MemoryMB = remaining(limit.MemoryMB, quotaMemoryUsage(used))
	rem.MemoryMaxMB = remaining(limit.MemoryMaxMB, used.MemoryMaxMB)
	rem.Allocations = remaining(limit.Allocations, used.Allocations)
	return rem
}

// MaxAllocs returns how many more allocations of the given size fit into the
// remaining capacity, or -1 if the number is unlimited. This is the quota
// ceiling used by the autoscaler before scaling up.
func (r *QuotaRemaining) MaxAllocs(per QuotaResources) int64 {
	if r == nil {
		return -1
	}
	result := int64(-1)
	apply := func(remaining *int64, size int64) {
		if remaining == nil || size <= 0 {
			return
		}
		n := max(*remaining, 0) / size
		if result < 0 || n < result {
			result = n
		}
	}
	apply(r.CPU, per.CPU)
	apply(r.MemoryMB, quotaMemoryUsage(per))
	apply(r.MemoryMaxMB, per.MemoryMaxMB)
	apply(r.Allocations, max(per.Allocations, 1))
	return result
}

// Add returns the sum of both vectors.
func (r QuotaResources) Add(o QuotaResources) QuotaResources {
	return QuotaResources{
		CPU:         r.CPU + o.CPU,
		MemoryMB:    r.MemoryMB + o.MemoryMB,
		MemoryMaxMB: r.MemoryMaxMB + o.MemoryMaxMB,
		Allocations: r.Allocations + o.Allocations,
	}
}

// Sub returns the difference of both vectors.
func (r QuotaResources) Sub(o QuotaResources) QuotaResources {
	return QuotaResources{
		CPU:         r.CPU - o.CPU,
		MemoryMB:    r.MemoryMB - o.MemoryMB,
		MemoryMaxMB: r.MemoryMaxMB - o.MemoryMaxMB,
		Allocations: r.Allocations - o.Allocations,
	}
}

// Mul returns the vector multiplied by n.
func (r QuotaResources) Mul(n int64) QuotaResources {
	return QuotaResources{
		CPU:         r.CPU * n,
		MemoryMB:    r.MemoryMB * n,
		MemoryMaxMB: r.MemoryMaxMB * n,
		Allocations: r.Allocations * n,
	}
}

// IsZero returns true if all dimensions are zero.
func (r QuotaResources) IsZero() bool {
	return r == QuotaResources{}
}

// quotaTaskUsage is the usage of a single task used when aggregating the
// tasks of an allocation.
type quotaTaskUsage struct {
	cpu, memory, memoryMax int64
}

func (u *quotaTaskUsage) add(o quotaTaskUsage) {
	u.cpu += o.cpu
	u.memory += o.memory
	u.memoryMax += o.memoryMax
}

func (u *quotaTaskUsage) maxWith(o quotaTaskUsage) {
	u.cpu = max(u.cpu, o.cpu)
	u.memory = max(u.memory, o.memory)
	u.memoryMax = max(u.memoryMax, o.memoryMax)
}

// aggregateQuotaTaskUsage sums the usage of the tasks of an allocation taking
// the task lifecycle into account the same way the scheduler does for node
// fit: ephemeral prestart and poststop tasks don't run at the same time as
// the main tasks. Poststart tasks are counted as main tasks.
func aggregateQuotaTaskUsage(tasks map[string]quotaTaskUsage, lifecycles map[string]*TaskLifecycleConfig) quotaTaskUsage {
	var sidecar, ephemeral, main, poststop quotaTaskUsage
	for name, u := range tasks {
		lc := lifecycles[name]
		switch {
		case lc == nil:
			main.add(u)
		case lc.Hook == TaskLifecycleHookPrestart && lc.Sidecar:
			sidecar.add(u)
		case lc.Hook == TaskLifecycleHookPrestart:
			ephemeral.add(u)
		case lc.Hook == TaskLifecycleHookPoststop:
			poststop.add(u)
		default:
			main.add(u)
		}
	}
	ephemeral.maxWith(main)
	ephemeral.maxWith(poststop)
	sidecar.add(ephemeral)
	return sidecar
}

// AllocQuotaResources returns the resources the allocation counts against a
// quota. Terminal allocations don't count. Device resources are not counted
// in this version.
func AllocQuotaResources(alloc *Allocation) QuotaResources {
	if alloc == nil || alloc.TerminalStatus() {
		return QuotaResources{}
	}

	var total quotaTaskUsage
	if ar := alloc.AllocatedResources; ar != nil {
		tasks := make(map[string]quotaTaskUsage, len(ar.Tasks))
		for name, tr := range ar.Tasks {
			if tr == nil {
				continue
			}
			memMax := tr.Memory.MemoryMaxMB
			if memMax == 0 {
				memMax = tr.Memory.MemoryMB
			}
			tasks[name] = quotaTaskUsage{
				cpu:       tr.Cpu.CpuShares,
				memory:    tr.Memory.MemoryMB,
				memoryMax: memMax,
			}
		}
		total = aggregateQuotaTaskUsage(tasks, ar.TaskLifecycles)
	} else {
		// Pre-0.9 allocations
		r := alloc.Resources
		if r == nil && alloc.TaskResources != nil {
			r = new(Resources)
			for _, tr := range alloc.TaskResources {
				r.Add(tr)
			}
		}
		if r != nil {
			memMax := int64(r.MemoryMaxMB)
			if memMax == 0 {
				memMax = int64(r.MemoryMB)
			}
			total = quotaTaskUsage{cpu: int64(r.CPU), memory: int64(r.MemoryMB), memoryMax: memMax}
		}
	}

	return QuotaResources{
		CPU:         total.cpu,
		MemoryMB:    total.memory,
		MemoryMaxMB: total.memoryMax,
		Allocations: 1,
	}
}

// TaskGroupQuotaResources estimates the resources one allocation of the task
// group will count against a quota. sharesPerCore converts reserved cores to
// MHz; when it is zero reserved cores are not counted.
func TaskGroupQuotaResources(tg *TaskGroup, sharesPerCore int64) QuotaResources {
	if tg == nil {
		return QuotaResources{}
	}
	tasks := make(map[string]quotaTaskUsage, len(tg.Tasks))
	lifecycles := make(map[string]*TaskLifecycleConfig, len(tg.Tasks))
	for _, task := range tg.Tasks {
		if task == nil || task.Resources == nil {
			continue
		}
		r := task.Resources
		cpu := int64(r.CPU)
		if r.Cores > 0 {
			cpu = int64(r.Cores) * sharesPerCore
		}
		memMax := int64(r.MemoryMaxMB)
		if memMax == 0 {
			memMax = int64(r.MemoryMB)
		}
		tasks[task.Name] = quotaTaskUsage{cpu: cpu, memory: int64(r.MemoryMB), memoryMax: memMax}
		lifecycles[task.Name] = task.Lifecycle
	}
	total := aggregateQuotaTaskUsage(tasks, lifecycles)
	return QuotaResources{
		CPU:         total.cpu,
		MemoryMB:    total.memory,
		MemoryMaxMB: total.memoryMax,
		Allocations: 1,
	}
}

// TaskGroupUsesCores returns true if any task of the group reserves cores.
func TaskGroupUsesCores(tg *TaskGroup) bool {
	if tg == nil {
		return false
	}
	for _, task := range tg.Tasks {
		if task != nil && task.Resources != nil && task.Resources.Cores > 0 {
			return true
		}
	}
	return false
}

// JobQuotaDemand returns the total demand of the job (count x resources of
// every task group). Reserved cores are not converted to MHz since the nodes
// are not known at submission. System and sysbatch jobs have no fixed count
// and return zero.
func JobQuotaDemand(job *Job) QuotaResources {
	var total QuotaResources
	if job == nil || job.Type == JobTypeSystem || job.Type == JobTypeSysBatch {
		return total
	}
	for _, tg := range job.TaskGroups {
		total = total.Add(TaskGroupQuotaResources(tg, 0).Mul(int64(tg.Count)))
	}
	return total
}

// QuotaPlanDelta computes the change in the usage of a quota that applying the
// plan would cause. inQuota returns whether allocations of a namespace count
// against the quota and existing returns the current version of an
// allocation (nil if it doesn't exist yet). Both are called at most once per
// namespace and allocation respectively.
func QuotaPlanDelta(plan *Plan,
	inQuota func(namespace string) (bool, error),
	existing func(allocID string) (QuotaResources, error)) (QuotaResources, error) {

	var delta QuotaResources
	if plan == nil {
		return delta, nil
	}

	// Build the final version of every allocation touched by the plan. Later
	// maps win, matching the order the state store applies plan results.
	final := make(map[string]*Allocation)
	for _, m := range []map[string][]*Allocation{plan.NodeUpdate, plan.NodePreemptions, plan.NodeAllocation} {
		for _, allocs := range m {
			for _, alloc := range allocs {
				final[alloc.ID] = alloc
			}
		}
	}

	namespaces := make(map[string]bool)
	for id, alloc := range final {
		ns := alloc.Namespace
		if ns == "" && plan.Job != nil {
			ns = plan.Job.Namespace
		}
		counts, ok := namespaces[ns]
		if !ok {
			var err error
			counts, err = inQuota(ns)
			if err != nil {
				return delta, err
			}
			namespaces[ns] = counts
		}
		if !counts {
			continue
		}

		prev, err := existing(id)
		if err != nil {
			return delta, err
		}
		delta = delta.Add(AllocQuotaResources(alloc)).Sub(prev)
	}
	return delta, nil
}

// UsageResources returns the allocation usage recorded in a usage entry.
func (l *QuotaLimit) UsageResources() QuotaResources {
	return l.Resources()
}

// SetUsageResources records the allocation usage in a usage entry.
func (l *QuotaLimit) SetUsageResources(r QuotaResources) {
	if l.RegionLimit == nil {
		l.RegionLimit = &Resources{}
	}
	l.RegionLimit.CPU = int(r.CPU)
	l.RegionLimit.MemoryMB = int(r.MemoryMB)
	l.RegionLimit.MemoryMaxMB = int(r.MemoryMaxMB)
	l.Allocations = pointer.Of(int(r.Allocations))
}

// NewQuotaUsage returns an empty usage for the quota specification, with one
// entry for every limit of the given region.
func NewQuotaUsage(spec *QuotaSpec, region string) *QuotaUsage {
	u := &QuotaUsage{
		Name: spec.Name,
		Used: make(map[string]*QuotaLimit),
	}
	for _, l := range spec.Limits {
		if l == nil || l.Region != region {
			continue
		}
		entry := &QuotaLimit{
			Region:         l.Region,
			RegionLimit:    &Resources{},
			VariablesLimit: pointer.Of(0),
			Allocations:    pointer.Of(0),
			Hash:           copyBytes(l.Hash),
		}
		u.Used[l.HashKey()] = entry
	}
	return u
}

// GetID implements the IDGetter interface required for pagination.
func (u *QuotaUsage) GetID() string {
	return u.Name
}

// GetCreateIndex implements the CreateIndexGetter interface required for
// pagination.
func (u *QuotaUsage) GetCreateIndex() uint64 {
	return u.CreateIndex
}

// Copy returns a deep copy of the quota usage.
func (u *QuotaUsage) Copy() *QuotaUsage {
	if u == nil {
		return nil
	}
	nu := new(QuotaUsage)
	*nu = *u
	if u.Used != nil {
		nu.Used = make(map[string]*QuotaLimit, len(u.Used))
		for k, v := range u.Used {
			nu.Used[k] = v.Copy()
		}
	}
	return nu
}

// RegionUsage returns the usage entry of the given region or nil if there is
// none. Entries are visited in key order so the result is deterministic.
func (u *QuotaUsage) RegionUsage(region string) *QuotaLimit {
	if u == nil {
		return nil
	}
	keys := make([]string, 0, len(u.Used))
	for k := range u.Used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if l := u.Used[k]; l != nil && l.Region == region {
			return l
		}
	}
	return nil
}

// QuotaSpecUpsertRequest is used to create or update a set of quota
// specifications.
type QuotaSpecUpsertRequest struct {
	Quotas []*QuotaSpec
	WriteRequest
}

// QuotaSpecDeleteRequest is used to delete a set of quota specifications.
type QuotaSpecDeleteRequest struct {
	Names []string
	WriteRequest
}

// QuotaSpecListRequest is used to list quota specifications or usages.
type QuotaSpecListRequest struct {
	QueryOptions
}

// QuotaSpecListResponse is used for a quota specification list request.
type QuotaSpecListResponse struct {
	Quotas []*QuotaSpec
	QueryMeta
}

// QuotaSpecSpecificRequest is used to query a specific quota specification
// or usage.
type QuotaSpecSpecificRequest struct {
	Name string
	QueryOptions
}

// SingleQuotaSpecResponse is used to return a single quota specification.
type SingleQuotaSpecResponse struct {
	Quota *QuotaSpec
	QueryMeta
}

// QuotaUsageListResponse is used for a quota usage list request.
type QuotaUsageListResponse struct {
	Usages []*QuotaUsage
	QueryMeta
}

// SingleQuotaUsageResponse is used to return a single quota usage.
type SingleQuotaUsageResponse struct {
	Usage *QuotaUsage
	QueryMeta
}

// QuotaSpecEvent is the event stream payload of quota specification changes.
type QuotaSpecEvent struct {
	QuotaSpec *QuotaSpec
}

// QuotaUsageEvent is the event stream payload of quota exhaustion changes.
type QuotaUsageEvent struct {
	QuotaUsage *QuotaUsage

	// Dimensions are the dimensions that became exhausted (QuotaExhausted)
	// or were freed (QuotaFreed).
	Dimensions []string
}
