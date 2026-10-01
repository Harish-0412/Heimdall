# syntax=docker/dockerfile:1
ARG NODE_IMAGE=public.ecr.aws/docker/library/node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1

FROM ${NODE_IMAGE}
ENV NODE_ENV=production
WORKDIR /app
COPY package.json server.js index.html app.js ./
USER node
EXPOSE 3000
CMD ["node", "server.js"]
