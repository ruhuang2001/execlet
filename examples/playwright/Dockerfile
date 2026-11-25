FROM debian:12-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
ARG NODE_VERSION=24.22.3
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && case "$(dpkg --print-architecture)" in \
        amd64) arch=x64; sha=4f2b175a6d53a5c617a80dba2676238a0688cef28c6da4eba35c7d844e66fcfb ;; \
        arm64) arch=arm64; sha=6e51a2ada00f0011cc266f65d98922b403d7ad88f809ca2aa615a5fe1ce6e86c ;; \
        *) exit 1 ;; \
    esac \
    && curl -fsSL --retry 3 "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz" -o /tmp/node.tar.xz \
    && echo "$sha  /tmp/node.tar.xz" | sha256sum -c - \
    && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 \
    && rm /tmp/node.tar.xz \
    && useradd --create-home --uid 1000 runner \
    && rm -rf /var/lib/apt/lists/*
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
WORKDIR /opt/execlet-playwright
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
    && ./node_modules/.bin/playwright-core install --with-deps --only-shell chromium \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*
COPY screenshot.cjs ./
ENV HOME=/home/runner
USER runner
