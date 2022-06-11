FROM gcr.io/distroless/static:latest
LABEL maintainers="Kubernetes Authors"
LABEL description="NfsExport Validation Webhook"
ARG binary=./bin/nfsexport-validation-webhook

COPY ${binary} nfsexport-validation-webhook
ENTRYPOINT ["/nfsexport-validation-webhook"]
