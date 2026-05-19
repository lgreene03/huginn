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

# Expose HTTP port 80 internally
EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
