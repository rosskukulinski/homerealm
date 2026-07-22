FROM python:3.12-alpine
RUN apk add --no-cache docker-cli docker-cli-compose && pip install --no-cache-dir flask
COPY app.py /app/app.py
EXPOSE 8090
CMD ["python", "/app/app.py"]
