package registry

import (
	"slices"
	"testing"
)

func TestBaseImages(t *testing.T) {
	for _, c := range []struct {
		name       string
		dockerfile string
		args       map[string]string
		refs       []string
		unresolved []string
	}{
		{"single", "FROM python:3.12-slim\nRUN pip install x\n", nil, []string{"python:3.12-slim"}, nil},
		{"multi-stage", `
# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
RUN go build -o /app .
FROM node:22-alpine as Web
RUN npm ci
from build AS test
FROM gcr.io/distroless/static:nonroot
COPY --from=build /app /app
COPY --from=web /dist /dist
`, nil, []string{"golang:1.24", "node:22-alpine", "gcr.io/distroless/static:nonroot"}, nil},
		{"scratch and duplicates", "FROM alpine:3.20 AS certs\nFROM scratch\nFROM alpine:3.20\n", nil,
			[]string{"alpine:3.20"}, nil},
		{"arg defaults", `
ARG REGISTRY=ghcr.io
ARG PG_MAJOR=16
ARG IMAGE="${REGISTRY}/acme/base"
ARG VARIANT
FROM postgres:${PG_MAJOR}-alpine
FROM $IMAGE:1
FROM node:${NODE:-20}
FROM debian:${VARIANT:-bookworm}-slim
`, nil, []string{"postgres:16-alpine", "ghcr.io/acme/base:1", "node:20", "debian:bookworm-slim"}, nil},
		{"build args override", "ARG PG_MAJOR=16\nArg TAG\nFROM postgres:$PG_MAJOR\nFROM app:${TAG}\n",
			map[string]string{"PG_MAJOR": "17", "TAG": "v2"}, []string{"postgres:17", "app:v2"}, nil},
		{"stage ARGs don't reach later FROMs", "FROM alpine\nARG V=1\nFROM busybox:${V}\n", nil,
			[]string{"alpine"}, []string{"busybox:${V}"}},
		{"unresolved", "ARG VERSION\nFROM node:${VERSION}\nFROM $BASE\nFROM ok:1\n", nil,
			[]string{"ok:1"}, []string{"node:${VERSION}", "$BASE"}},
		{"continuations, comments, CRLF", "FROM \\\r\n  # a comment\r\n  --platform=linux/amd64 \\\r\n  redis:7 \\\r\n  AS cache\r\nFROM cache\r\n", nil,
			[]string{"redis:7"}, nil},
		{"heredoc body is not a FROM", "FROM python:3.12\nRUN <<EOF python3\nfrom os import path\nFROM evil:1\nEOF\nFROM nginx:1.27\n", nil,
			[]string{"python:3.12", "nginx:1.27"}, nil},
	} {
		refs, unresolved := BaseImages(c.dockerfile, c.args)
		if !slices.Equal(refs, c.refs) || !slices.Equal(unresolved, c.unresolved) {
			t.Errorf("%s: got %q %q, want %q %q", c.name, refs, unresolved, c.refs, c.unresolved)
		}
	}
}
