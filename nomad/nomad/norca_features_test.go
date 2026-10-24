// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/serf/serf"
	"github.com/shoenig/test/must"
)

func makeNorcaMember(norcaVersion string, status serf.MemberStatus) serf.Member {
	m := makeMember("1.6.5", status)
	if norcaVersion != "" {
		m.Tags[norcaVersionTag] = norcaVersion
	}
	return m
}

func TestServersMeetMinimumNorcaVersion(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name        string
		members     []serf.Member
		checkFailed bool
		expected    bool
	}{
		{
			name:     "single server at the minimum",
			members:  []serf.Member{makeNorcaMember("0.1.0", serf.StatusAlive)},
			expected: true,
		},
		{
			name:     "prerelease of the minimum",
			members:  []serf.Member{makeNorcaMember("0.1.0-dev", serf.StatusAlive)},
			expected: true,
		},
		{
			name:     "newer server",
			members:  []serf.Member{makeNorcaMember("0.2.0", serf.StatusAlive)},
			expected: true,
		},
		{
			name:     "older server",
			members:  []serf.Member{makeNorcaMember("0.0.9", serf.StatusAlive)},
			expected: false,
		},
		{
			name: "upstream Nomad server without the tag",
			members: []serf.Member{
				makeNorcaMember("0.1.0", serf.StatusAlive),
				makeNorcaMember("", serf.StatusAlive),
			},
			expected: false,
		},
		{
			name:     "invalid tag",
			members:  []serf.Member{makeNorcaMember("not-a-version", serf.StatusAlive)},
			expected: false,
		},
		{
			name: "failed upstream server ignored",
			members: []serf.Member{
				makeNorcaMember("0.1.0", serf.StatusAlive),
				makeNorcaMember("", serf.StatusFailed),
			},
			expected: true,
		},
		{
			name: "failed upstream server checked",
			members: []serf.Member{
				makeNorcaMember("0.1.0", serf.StatusAlive),
				makeNorcaMember("", serf.StatusFailed),
			},
			checkFailed: true,
			expected:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must.Eq(t, tc.expected, ServersMeetMinimumNorcaVersion(tc.members, AllRegions, tc.checkFailed))
		})
	}

	// A member of another region does not count
	other := makeNorcaMember("", serf.StatusAlive)
	other.Tags["region"] = "other"
	must.True(t, ServersMeetMinimumNorcaVersion(
		[]serf.Member{makeNorcaMember("0.1.0", serf.StatusAlive), other}, "aws", false))
}

func TestServer_AdvertisesNorcaVersion(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()

	members := s.Members()
	must.Len(t, 1, members)
	ok, parts := isNomadServer(members[0])
	must.True(t, ok)
	must.NotNil(t, parts.NorcaVersion)
	must.True(t, ServersMeetMinimumNorcaVersion(members, s.Region(), false))
}
