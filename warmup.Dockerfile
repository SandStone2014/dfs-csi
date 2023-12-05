FROM golang:1.17-buster as builder
WORKDIR /workspace
COPY . /workspace
RUN make warmup

FROM alpine:3.16
COPY --from=builder /workspace/bin/warmup /warmup
ENTRYPOINT ["/warmup"]
