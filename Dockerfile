FROM golang AS builder

WORKDIR /builder

ENV CGO_ENABLED=0

COPY go.mod go.sum /builder/
RUN go mod download

COPY . /builder/
RUN go build -v -o /etcd-config-dispenser

FROM scratch

COPY --from=builder /etcd-config-dispenser /bin/etcd-config-dispenser
