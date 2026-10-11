# syntax=docker/dockerfile:1
# chipagent image. Build from the agent/ directory:
#   docker build -t chipagent agent/
# NVIDIA hosts: docker run --gpus all -p 9400:9400 chipagent
# AMD/Intel hosts: docker run -p 9400:9400 chipagent   (reads /sys, visible by default)
FROM debian:bookworm-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends cmake g++ make \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN rm -rf build && cmake -S . -B build -DCMAKE_BUILD_TYPE=Release \
    && cmake --build build -j \
    && ctest --test-dir build --output-on-failure

FROM debian:bookworm-slim
COPY --from=build /src/build/chipagent /usr/local/bin/chipagent
# Example plugins for chips without built-in support (mount the vendor tool too).
COPY plugins /opt/chipagent/plugins
# Ask the NVIDIA container runtime to mount libnvidia-ml (NVML) into the container.
ENV NVIDIA_VISIBLE_DEVICES=all NVIDIA_DRIVER_CAPABILITIES=utility
USER 65532:65532
EXPOSE 9400
ENTRYPOINT ["/usr/local/bin/chipagent"]
