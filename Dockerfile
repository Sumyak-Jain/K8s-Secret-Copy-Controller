FROM golang:1.20 AS build
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -o /manager main.go

FROM gcr.io/distroless/static
COPY --from=build /manager /manager
ENTRYPOINT ["/manager"]