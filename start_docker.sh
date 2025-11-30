#!/bin/bash

IMAGE_NAME="webhook-proxy"
CONTAINER_NAME="webhook-proxy"
PORT=$(grep "^PORT=" .env | cut -d '=' -f2)

GREEN="\033[0;32m"
YELLOW="\033[1;33m"
RED="\033[0;31m"
NC="\033[0m"

echo -e "${GREEN}Webhook Proxy Runner (Always Build)${NC}"
echo "-----------------------------------"

if ! command -v docker &> /dev/null; then
    echo -e "${RED}❌ Docker is not installed.${NC}"
    exit 1
fi

if [ ! -f .env ]; then
    echo -e "${RED}❌ .env file not found.${NC}"
    exit 1
fi

if [ -z "$PORT" ]; then
    PORT=8080
fi

echo -e "${YELLOW}→ Building Docker image ${IMAGE_NAME}...${NC}"
docker build --no-cache -t "$IMAGE_NAME" .

if [ $? -ne 0 ]; then
    echo -e "${RED}❌ Build failed.${NC}"
    exit 1
fi

RUNNING=$(docker ps -q -f name="$CONTAINER_NAME")
if [ -n "$RUNNING" ]; then
    echo -e "${YELLOW}→ Stopping existing container...${NC}"
    docker stop "$CONTAINER_NAME"
fi

EXISTS=$(docker ps -aq -f name="$CONTAINER_NAME")
if [ -n "$EXISTS" ]; then
    echo -e "${YELLOW}→ Removing old container...${NC}"
    docker rm "$CONTAINER_NAME"
fi

echo -e "${YELLOW}→ Starting container on port $PORT...${NC}"

docker run -d \
    --name "$CONTAINER_NAME" \
    --env-file .env \
    -p "$PORT":"$PORT" \
    "$IMAGE_NAME"

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✔ Container running successfully!${NC}"
    echo -e "URL: ${GREEN}http://localhost:$PORT${NC}"
else
    echo -e "${RED}❌ Failed to start container.${NC}"
    exit 1
fi
