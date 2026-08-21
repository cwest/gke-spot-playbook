FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /agent .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agent /agent
ENTRYPOINT ["/agent"]
