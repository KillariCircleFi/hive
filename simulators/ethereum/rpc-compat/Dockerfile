# This simulation runs JSON-RPC API tests.
FROM golang:1-alpine as builder
RUN apk add --update git ca-certificates gcc musl-dev linux-headers

# Clone and build geth.
RUN git clone --depth 1 https://github.com/s1na/go-ethereum.git --branch multicall /geth
RUN cd /geth && go run build/ci.go install -static ./cmd/geth

# Fill the tests via rpctestgen.
RUN git clone --depth 1 https://github.com/DarkFlorist/rpctestgen.git --branch eth_simulatev1 /rpctestgen
RUN cd /rpctestgen && go build .
RUN cd /rpctestgen && ./rpctestgen --bin ../geth/build/bin/geth -chain ./chain --tests simulate


# To run local tests, copy the directory into the same as the simulator and
# uncomment the line below
# ADD tests /execution-apis/tests

# Build the simulator executable.
ADD . /source
WORKDIR /source
RUN go build -v .

# Build the simulator run container.
FROM alpine:latest
ADD . /source
WORKDIR /source
COPY --from=builder /source/rpc-compat .
COPY --from=builder /rpctestgen/tests ./tests

ENTRYPOINT ["./rpc-compat"]
