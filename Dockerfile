FROM python:3.13-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    DATABASE_PATH=/data/next-chapter.sqlite3
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt \
    && groupadd --gid 10001 reader \
    && useradd --uid 10001 --gid reader --no-create-home reader \
    && mkdir /data && chown reader:reader /data
COPY next_chapter ./next_chapter
COPY examples ./examples
USER 10001:10001
EXPOSE 8000
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=3)"
CMD ["gunicorn", "--bind", "0.0.0.0:8000", "--workers", "1", "--threads", "4", "--timeout", "60", "--worker-tmp-dir", "/tmp", "next_chapter:create_app()"]
