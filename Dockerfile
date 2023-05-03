# development docker image for compose
FROM golang:1.20-buster as builder

RUN apt update && apt upgrade -y

COPY . /root/src

WORKDIR /root/src/cmd/proxyd
RUN go build

# FROM gcr.io/distroless/base-debian11
FROM ubuntu:22.10

WORKDIR /
COPY --from=builder /root/src/cmd/proxyd/proxyd .

ENTRYPOINT ["/proxyd"]
