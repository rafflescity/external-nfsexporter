FROM gcr.io/distroless/static:latest
LABEL maintainers="Kubernetes Authors"
LABEL description="CSI External NfsExportter Sidecar"
ARG binary=./bin/csi-nfsexporter

COPY ${binary} csi-nfsexporter
ENTRYPOINT ["/csi-nfsexporter"]
