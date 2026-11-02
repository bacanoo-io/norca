#!/usr/local/bin/dumb-init /bin/sh
# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Script created following HashiCorp's model for Consul: 
# https://github.com/hashicorp/docker-consul/blob/master/0.X/docker-entrypoint.sh
# Comments in this file originate from the project above, simply replacing 'Consul' with 'Nomad'.
set -e

# Note above that we run dumb-init as PID 1 in order to reap zombie processes
# as well as forward signals to all processes in its session. Normally, sh
# wouldn't do either of these functions so we'd leak zombies as well as do
# unclean termination of all our sub-processes.

# NORCA_DATA_DIR is exposed as a volume for possible persistent storage. The
# NORCA_CONFIG_DIR isn't exposed as a volume but you can compose additional
# config files in there if you use this image as a base, or use NORCA_LOCAL_CONFIG
# below.
NORCA_DATA_DIR=/nomad/data
NORCA_CONFIG_DIR=/etc/nomad

# You can also set the NORCA_LOCAL_CONFIG environment variable to pass some
# Nomad configuration JSON without having to bind any volumes.
if [ -n "$NORCA_LOCAL_CONFIG" ]; then
	echo "$NORCA_LOCAL_CONFIG" > "$NORCA_CONFIG_DIR/local.json"
fi

# If the user is trying to run Nomad directly with some arguments, then
# pass them to Nomad.
if [ "${1:0:1}" = '-' ]; then
    set -- nomad "$@"
fi

# Look for Nomad subcommands.
if [ "$1" = 'agent' ]; then
    shift
    set -- nomad agent \
        -data-dir="$NORCA_DATA_DIR" \
        -config="$NORCA_CONFIG_DIR" \
        "$@"
elif [ "$1" = 'version' ]; then
    # This needs a special case because there's no help output.
    set -- nomad "$@"
elif nomad --help "$1" 2>&1 | grep -q "nomad $1"; then
    # We can't use the return code to check for the existence of a subcommand, so
    # we have to use grep to look for a pattern in the help output.
    set -- nomad "$@"
fi

# If we are running Nomad, make sure it executes as the proper user.
if [ "$1" = 'nomad' ]; then
    # If the data or config dirs are bind mounted then chown them.
    # Note: This checks for root ownership as that's the most common case.
    if [ "$(stat -c %u /nomad/data)" != "$(id -u root)" ]; then
        chown root:root /etc/nomad
    fi

    # If requested, set the capability to bind to privileged ports before
    # we drop to the non-root user. Note that this doesn't work with all
    # storage drivers (it won't work with AUFS).
    if [ ! -z ${NOMAD+x} ]; then
        setcap "cap_net_bind_service=+ep" /bin/nomad
    fi

    set -- gosu root "$@"
fi

exec "$@"
