FROM debian:12-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update && apt-get install -y --no-install-recommends bash python3 coreutils ca-certificates \
    && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 1000 -s /bin/bash user
ENV HOME=/home/user
WORKDIR /home/user
USER user
CMD ["/bin/bash"]
