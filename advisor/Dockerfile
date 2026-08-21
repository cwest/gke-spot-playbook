# Build the advisor as a static binary and ship it on distroless. The reconciler
# runs on a schedule in-cluster with cluster-write permissions, so the image
# carries no shell and no package manager.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/capacity-advisor ./cmd/capacity-advisor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/capacity-advisor /capacity-advisor
ENTRYPOINT ["/capacity-advisor"]
