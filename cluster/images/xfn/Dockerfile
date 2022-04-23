# TODO(negz): Download crun via the Makefile instead?
FROM curlimages/curl@sha256:cfdeba7f88bb85f6c87f2ec9135115b523a1c24943976a61fbf59c4f2eafd78e AS curl

ARG CRUN_VERSION=1.6
ARG TARGETOS
ARG TARGETARCH

RUN curl -fSSL -o /tmp/crun https://github.com/containers/crun/releases/download/${CRUN_VERSION}/crun-${CRUN_VERSION}-${TARGETOS}-${TARGETARCH}-disable-systemd
RUN chmod +x /tmp/crun

FROM gcr.io/distroless/static@sha256:d2b0ec3141031720cf5eedef3493b8e129bc91935a43b50562fbe5429878d96b

ARG TARGETOS
ARG TARGETARCH

COPY --from=curl /tmp/crun /usr/local/bin/
ADD bin/${TARGETOS}\_${TARGETARCH}/xfn /usr/local/bin/
USER 65532
ENTRYPOINT ["xfn"]
