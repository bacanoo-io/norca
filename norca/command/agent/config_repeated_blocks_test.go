// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// An unknown key (a typo) in a repeated block is an error, like in the other
// blocks: the unused keys of the configuration are not looked for in slices,
// so each of these blocks is checked by itself.
func TestConfig_Parse_RepeatedBlocks_UnknownKeys(t *testing.T) {
	ci.Parallel(t)

	parse := func(t *testing.T, hcl string) error {
		path := filepath.Join(t.TempDir(), "agent.hcl")
		must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
		_, err := ParseConfigFile(path)
		return err
	}

	bad := map[string]string{
		`server { autoscaler { node_scaling { pool "x" { max_ndoes = 3 } } } }`:                    "max_ndoes",
		`server { autoscaler { apm "p" { drvier = "prometheus" } } }`:                              "drvier",
		`server { autoscaler { strategy "s" { drvier = "custom" } } }`:                             "drvier",
		`client { secrets { vault_kv "kv/data/" { pth = "apps/" } } }`:                             "pth",
		`client { secrets { route "kv/" { provdier = "norca" } } }`:                                "provdier",
		`server { autoscaler { node_scaling { pool "x" { max_nodes = 3 } pool "y" { a = 1 } } } }`: "server.autoscaler.node_scaling.pool.y",
	}
	for hcl, want := range bad {
		must.ErrorContains(t, parse(t, hcl), want, must.Sprint(hcl))
	}

	good := `
server {
  autoscaler {
    apm "p" {
      driver = "prometheus"
      args   = ["-x"]
      config {
        address = "http://127.0.0.1:9090"
      }
    }
    strategy "s" {
      driver = "custom"
      config = {
        key = "value"
      }
    }
    node_scaling {
      pool "x" {
        max_nodes = 3
        max_step  = 1
      }
    }
  }
}
client {
  secrets {
    route "kv/" {
      provider = "norca"
    }
    vault_kv "kv/data/" {
      kv_version = 2
      path       = "apps/"
    }
  }
}
`
	must.NoError(t, parse(t, good))
}
