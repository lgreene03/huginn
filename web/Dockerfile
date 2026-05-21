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

# Serve via high-performance Nginx web server
FROM nginx:alpine

# Copy bundled application assets
COPY --from=build /app/dist /usr/share/nginx/html

# Production nginx config: SPA try_files fallback, SSE-safe API proxy,
# CORS at proxy layer. HUGINN_UPSTREAM is substituted at container start
# by envsubst (default: huginn:8081 for docker-compose).
COPY nginx.conf /etc/nginx/templates/default.conf.template

# Default upstream — override with -e HUGINN_UPSTREAM=<host:port>
ENV HUGINN_UPSTREAM=huginn:8081

# Expose HTTP port 80 internally
EXPOSE 80

# nginx official image auto-runs envsubst on /etc/nginx/templates/*.template
# and writes the result to /etc/nginx/conf.d/ before starting nginx.
CMD ["nginx", "-g", "daemon off;"]
