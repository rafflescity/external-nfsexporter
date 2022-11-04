FROM alpine:3.14

RUN set -x && \
    echo http://mirrors.aliyun.com/alpine/edge/main > /etc/apk/repositories && \
    echo http://mirrors.aliyun.com/alpine/edge/testing >> /etc/apk/repositories && \
    apk add --no-cache kubectl gettext

COPY volume-nfs-provisioner /usr/bin/

ADD tmpl /tmpl

ADD cmd/provisioner/ /usr/bin/

ENTRYPOINT [ "volume-nfs-provisioner" ]