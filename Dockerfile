FROM python:3.11-slim AS builder

# Install system dependencies for building
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    git \
    gcc \
    libc-dev \
    libffi-dev \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

# Install uv
RUN pip install uv

WORKDIR /app

# Create a virtual environment
RUN uv venv

# Copy dependencies
COPY pyproject.toml .

# Install dependencies using uv
# We add build isolation settings via pyproject.toml now
RUN uv pip install -r pyproject.toml

# Final stage
FROM python:3.11-slim

# Install system dependencies for runtime
# - docker.io: For restarting sibling containers
# - gnupg: Required by proton-python-client
# - curl: To download docker-compose
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    docker.io \
    gnupg \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Install docker-compose
RUN curl -L "https://github.com/docker/compose/releases/download/v2.24.6/docker-compose-linux-x86_64" -o /usr/local/bin/docker-compose && \
    chmod +x /usr/local/bin/docker-compose

WORKDIR /app

# Copy virtual environment from builder
COPY --from=builder /app/.venv /app/.venv

# Activate virtual environment in path
ENV PATH="/app/.venv/bin:$PATH"

# Copy script
COPY manager.py .

# Run unbuffered
CMD ["python", "-u", "manager.py"]
