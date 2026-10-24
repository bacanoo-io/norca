#!/usr/bin/env ash
# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0


case "$1" in
  "agent" )
    if [[ -z "${NORCA_SKIP_DOCKER_IMAGE_WARN}" && -z "${NOMAD_SKIP_DOCKER_IMAGE_WARN}" ]]
    then
      echo "====================================================================================="
      echo "!! Running Norca clients inside Docker containers is not supported.                !!"
      echo "!! Set the NORCA_SKIP_DOCKER_IMAGE_WARN environment variable to skip this warning. !!"
      echo "====================================================================================="
      echo ""
      sleep 2
    fi
esac

exec norca "$@"
