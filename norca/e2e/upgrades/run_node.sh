# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

#/bin/bash
# this script runs a nomad node (client/server)
# first arg is the binary and second arg is of the format (<client|server><num>).
# this is only meant to be used within the context of the cluster created in run_cluster.sh 
# e.g usage ./run_node.sh nomad client1
if [ "$#" -ne 2 ]; then
    echo "Expected usage ./run_node.sh /path/to/binary <client|server><1|2>"
    exit 255
fi
NORCA_BINARY=$1
NODE=$2
( $NORCA_BINARY agent -config=${NODE}.hcl 2>&1 | tee -a "/tmp/$NODE/log" ; echo "Exit code: $?" >> "/tmp/$NODE/log" ) &
