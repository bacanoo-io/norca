// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package pluginrpc benchmarks the connection between the agent and a task
// driver plugin process (B3 of the storage update plan): a unary call and a
// stream of task statistics through the plugin protocol, with the mock
// driver behind it, so that the protocol is what is measured.
//
//	go test -run '^$' -bench . -benchmem -count 10 ./internal/benchmarks/pluginrpc/
package pluginrpc

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/drivers/mock"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/helper/testtask"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	basePlug "github.com/bacanoo-io/norca/norca/plugins/base"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	dtestutil "github.com/bacanoo-io/norca/norca/plugins/drivers/testutils"
)

// startTask starts a long running mock task behind the plugin protocol.
func startTask(b *testing.B) (*dtestutil.DriverHarness, string) {
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	harness := dtestutil.NewDriverHarness(b, &statsDriver{mock.NewMockDriver(ctx, testlog.HCLogger(b))})
	b.Cleanup(harness.Kill)

	var data []byte
	if err := basePlug.MsgPackEncode(&data, &mock.Config{}); err != nil {
		b.Fatal(err)
	}
	if err := harness.SetConfig(&basePlug.Config{PluginConfig: data}); err != nil {
		b.Fatal(err)
	}

	task := &drivers.TaskConfig{AllocID: uuid.Generate(), ID: uuid.Generate(), Name: "bench", Env: map[string]string{}}
	if err := task.EncodeConcreteDriverConfig(&map[string]any{"run_for": "1h"}); err != nil {
		b.Fatal(err)
	}
	testtask.SetTaskConfigEnv(task)
	b.Cleanup(harness.MkAllocDir(task, true))
	if _, _, err := harness.StartTask(task); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = harness.DestroyTask(task.ID, true) })
	return harness, task.ID
}

// BenchmarkInspectTask is one unary call: request, plugin side, response.
func BenchmarkInspectTask(b *testing.B) {
	harness, id := startTask(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := harness.InspectTask(id); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTaskStatsStream receives task statistics from one open stream,
// as the client does for every running task. One iteration is one message;
// the plugin sends as fast as the stream takes them (10 µs interval).
func BenchmarkTaskStatsStream(b *testing.B) {
	harness, id := startTask(b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := harness.TaskStats(ctx, id, 10*time.Microsecond)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := <-ch; !ok {
			b.Fatal("stats stream closed")
		}
	}
}

// statsDriver is the mock driver with task statistics as a real driver sends
// them (CPU and memory, at the requested interval); the mock's own stats
// lack the CPU part and are sent once.
type statsDriver struct {
	drivers.DriverPlugin
}

func (d *statsDriver) TaskStats(ctx context.Context, _ string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	ch := make(chan *drivers.TaskResourceUsage)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			u := &drivers.TaskResourceUsage{
				ResourceUsage: &drivers.ResourceUsage{
					MemoryStats: &drivers.MemoryStats{RSS: 64 << 20, Cache: 8 << 20, Measured: []string{"RSS", "Cache"}},
					CpuStats:    &drivers.CpuStats{SystemMode: 1.5, UserMode: 3.5, TotalTicks: 120, Percent: 5, Measured: []string{"System Mode", "User Mode", "Percent"}},
				},
				Timestamp: time.Now().UnixNano(),
			}
			select {
			case <-ctx.Done():
				return
			case ch <- u:
			}
		}
	}()
	return ch, nil
}
