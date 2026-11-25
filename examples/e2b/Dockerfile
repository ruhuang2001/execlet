FROM debian:12-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
COPY --from=ghcr.io/astral-sh/uv:0.13.10@sha256:a3ea1357342c1a4bf9a0c53a3deb1b55379aefa037338d8113d5771efa01774f /uv /usr/local/bin/uv
RUN apt-get update && apt-get install -y --no-install-recommends bash python3 python3-venv coreutils ca-certificates \
    && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 1000 -s /bin/bash user
ENV HOME=/home/user
WORKDIR /home/user
USER user
CMD ["/bin/bash"]
