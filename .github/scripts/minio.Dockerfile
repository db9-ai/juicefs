FROM scratch
ENV HOME=/root
COPY minio /minio
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/minio"]
