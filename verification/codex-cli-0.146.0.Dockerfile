FROM node:22.23.1-trixie-slim

RUN npm install --global @openai/codex@0.146.0 \
    && codex --version

ENTRYPOINT ["codex"]
