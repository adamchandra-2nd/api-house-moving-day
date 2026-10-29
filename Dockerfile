# Build the API as a static binary, then ship only the binary.
#
# Railway can also build a Go service without a Dockerfile, but its autodetect
# has to guess which package in the repo is the program. This repo has one
# `main` under cmd/server and a dozen libraries under internal/, so saying it
# outright is cheaper than discovering the guess was wrong from a failed deploy.

FROM golang:1.24-alpine AS build
WORKDIR /src

# Dependencies first: this layer is rebuilt only when the module files change,
# so ordinary code edits do not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 makes the binary self-contained, which is what lets the final
# stage be an image with no libc in it at all.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

# The migrations are not read by the program -- they are run by hand in the
# Supabase SQL editor -- but carrying them means the running image and the SQL
# that shaped its database are the same artifact, which is worth the few KB.
COPY --from=build /out/server /server
COPY migrations /migrations

# Documentation only. Railway injects PORT and the server binds whatever it
# says; see listenAddr in internal/config.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/server"]
