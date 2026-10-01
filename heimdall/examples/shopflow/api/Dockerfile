# syntax=docker/dockerfile:1
# Base images pinned by digest (from ECR Public's mirror of Docker Official Images).
ARG NODE_IMAGE=public.ecr.aws/docker/library/node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1

FROM ${NODE_IMAGE} AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund

FROM ${NODE_IMAGE}
# Heimdall runs containers as an arbitrary non-root user with a read-only root
# filesystem; npm's cache and logs must therefore live under /tmp.
ENV NODE_ENV=production \
    npm_config_cache=/tmp/.npm \
    npm_config_update_notifier=false
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY package.json ./
COPY src ./src
USER node
EXPOSE 8080
CMD ["node", "src/server.js"]
