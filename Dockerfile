# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 1: build libacars from source
# ---------------------------------------------------------------------------
FROM ubuntu:24.04 AS libacars-builder

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        cmake \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

ARG LIBACARS_VERSION=2.2.1
RUN curl -fsSL "https://github.com/szpajder/libacars/archive/refs/tags/v${LIBACARS_VERSION}.tar.gz" \
        | tar -xz -C /tmp && \
    cmake -S "/tmp/libacars-${LIBACARS_VERSION}" -B /tmp/libacars-build \
          -DCMAKE_BUILD_TYPE=Release \
          -DCMAKE_INSTALL_PREFIX=/usr/local && \
    cmake --build /tmp/libacars-build --parallel "$(nproc)" && \
    cmake --install /tmp/libacars-build

# ---------------------------------------------------------------------------
# Stage 2: build dumphfdl from source
# ---------------------------------------------------------------------------
FROM ubuntu:24.04 AS dumphfdl-builder

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends software-properties-common && \
    add-apt-repository universe && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential \
        cmake \
        pkg-config \
        git \
        ca-certificates \
        libglib2.0-dev \
        libconfig++-dev \
        libliquid-dev \
        libfftw3-dev \
        libsoapysdr-dev \
    && rm -rf /var/lib/apt/lists/*

# Copy libacars headers and library from the builder stage
COPY --from=libacars-builder /usr/local/include /usr/local/include
COPY --from=libacars-builder /usr/local/lib     /usr/local/lib
RUN ldconfig

ARG DUMPHFDL_VERSION=master
RUN git clone --depth 1 --branch "${DUMPHFDL_VERSION}" \
        https://github.com/szpajder/dumphfdl.git /tmp/dumphfdl && \
    cmake -S /tmp/dumphfdl -B /tmp/dumphfdl-build \
          -DCMAKE_BUILD_TYPE=Release \
          -DCMAKE_INSTALL_PREFIX=/usr/local && \
    cmake --build /tmp/dumphfdl-build --parallel "$(nproc)" && \
    cmake --install /tmp/dumphfdl-build

# ---------------------------------------------------------------------------
# Stage 3: build ubersdr_iq and hfdl_launcher
# ---------------------------------------------------------------------------
FROM golang:1.22-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_iq . && \
    go build -o /out/hfdl_launcher ./cmd/hfdl_launcher/

# ---------------------------------------------------------------------------
# Stage 4: runtime image
# ---------------------------------------------------------------------------
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Enable universe for libliquid1; install all other runtime deps from main
RUN apt-get update && \
    apt-get install -y --no-install-recommends software-properties-common && \
    add-apt-repository universe && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        libliquid1 \
        libfftw3-single3 \
        libglib2.0-0 \
        libconfig++9v5 \
        libjansson4 \
        libxml2 \
        libsoapysdr0.8 \
        libfec0 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false hfdl

# Copy libacars built from source
COPY --from=libacars-builder /usr/local/lib/libacars-2.so* /usr/local/lib/

# Copy dumphfdl built from source
COPY --from=dumphfdl-builder /usr/local/bin/dumphfdl /usr/local/bin/dumphfdl

# Copy Go binaries from builder
COPY --from=go-builder /out/ubersdr_iq    /usr/local/bin/ubersdr_iq
COPY --from=go-builder /out/hfdl_launcher /usr/local/bin/hfdl_launcher

# Copy static web files for the statistics dashboard
COPY static/ /usr/local/share/hfdl_launcher/static/

# Copy entrypoint script (translates env vars to hfdl_launcher flags)
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Create the default IQ recordings directory and ensure the hfdl user owns it.
# Users can volume-mount a host directory over /iq_recordings and set
# IQ_RECORD_DIR=/iq_recordings to persist WAV files on the host.
RUN ldconfig && \
    chmod +x /usr/local/bin/entrypoint.sh && \
    mkdir -p /iq_recordings && \
    chown hfdl:hfdl /iq_recordings

USER hfdl

# Expose the web statistics server port (default; override with WEB_PORT env var)
EXPOSE 6090

# hfdl_launcher is a long-running supervisor; verify it can print help
HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD ["/usr/local/bin/hfdl_launcher", "-help"] || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
