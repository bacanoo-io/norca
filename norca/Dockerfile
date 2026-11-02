# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0
# SPDX-FileCopyrightText: 2026 Norca contributors

# docker.io/library/busybox:1.36.0
# When pinning use the multi-arch manifest list, `docker buildx imagetools inspect ...`
FROM docker.io/library/busybox@sha256:9e2bbca079387d7965c3a9cee6d0c53f4f4e63ff7637877a83c4c05f2a666112 AS release

ARG PRODUCT_NAME=norca
ARG PRODUCT_VERSION
ARG PRODUCT_REVISION
# SOURCE_URL is the repository the image is built from; the release workflow
# passes the GitHub repository URL.
ARG SOURCE_URL
# TARGETARCH and TARGETOS are set automatically when --platform is provided.
ARG TARGETOS TARGETARCH

LABEL org.opencontainers.image.title="Norca"
LABEL org.opencontainers.image.version=${PRODUCT_VERSION}
LABEL org.opencontainers.image.revision=${PRODUCT_REVISION}
LABEL org.opencontainers.image.source=${SOURCE_URL}
LABEL org.opencontainers.image.licenses="MPL-2.0"
LABEL version=${PRODUCT_VERSION}
LABEL revision=${PRODUCT_REVISION}

# The license, the notice and the third-party notices accompany the binary
# (norca-doc content/docs/introduction/license.md, "Distribution"). The release workflow stages them in
# dist/doc/ from the repository root.
COPY dist/doc/LICENSE dist/doc/NOTICE dist/doc/THIRD_PARTY_NOTICES.md /usr/share/doc/norca/
COPY dist/$TARGETOS/$TARGETARCH/norca /bin/
COPY ./scripts/docker-entrypoint.sh /

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["help"]
