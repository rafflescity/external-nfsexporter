FROM gcr.io/distroless/static:latest
LABEL maintainers="Kubernetes Authors"
LABEL description="NfsExport Controller"
ARG binary=./bin/nfsexport-controller

COPY ${binary} nfsexport-controller
ENTRYPOINT ["/nfsexport-controller"]
