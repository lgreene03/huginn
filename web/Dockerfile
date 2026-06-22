# Multi-stage production Dockerfile for Huginn Admin Dashboard React app
FROM node:20-alpine AS build

WORKDIR /app

# Copy package configurations
COPY package*.json ./

# Install dependencies in clean mode
RUN npm ci

# Copy codebase
COPY . .

# Compile and bundle
RUN npm run build

# Serve via high-performance Nginx web server.
#
# Use the unprivileged nginx image: it runs as the non-root `nginx` user
# (UID 101), drops all setuid binaries, and listens on 8080 instead of the
# privileged port 80 — so no part of the container ever runs as root. The
# official `nginx:alpine` image launches its master process as root; this
# variant is the maintained, drop-in fix for that.
FROM nginxinc/nginx-unprivileged:alpine

# Copy bundled application assets
COPY --from=build /app/dist /usr/share/nginx/html

# Production nginx config: SPA try_files fallback, SSE-safe API proxy,
# CORS at proxy layer. HUGINN_UPSTREAM is substituted at container start
# by envsubst (default: huginn:8081 for docker-compose). The server block
# listens on 8080 (unprivileged).
COPY nginx.conf /etc/nginx/templates/default.conf.template

# Default upstream — override with -e HUGINN_UPSTREAM=<host:port>
ENV HUGINN_UPSTREAM=huginn:8081

# Run as the non-root nginx user shipped by the unprivileged image.
USER nginx

# Expose the unprivileged HTTP port internally.
EXPOSE 8080

# nginx official image auto-runs envsubst on /etc/nginx/templates/*.template
# and writes the result to /etc/nginx/conf.d/ before starting nginx.
CMD ["nginx", "-g", "daemon off;"]
