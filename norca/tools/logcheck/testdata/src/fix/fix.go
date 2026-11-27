// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fix

import (
	hclog "github.com/hashicorp/go-hclog"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

func f(logger hclog.Logger, alloc *structs.Allocation, job *structs.Job, jns structs.NamespacedID, err error) {
	logger.Warn("Failed to start.", "err", err)   // want `message ends with a period and starts` `key "err"`
	logger.Info("camel", "nodeID", "n1")          // want `key "nodeID"`
	logger.Info("alloc", "alloc_id", alloc)       // want `a whole Allocation is logged`
	logger.Info("job", "job", job.NamespacedID()) // want `key "job"` `a whole NamespacedID is logged`
	logger.Info("drain", "job_id", jns)           // want `a whole NamespacedID is logged`
}
