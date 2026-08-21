FROM pytorch/pytorch:2.4.1-cuda12.1-cudnn9-runtime
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY checkpoints.py train.py ./
ENV PYTHONUNBUFFERED=1
ENTRYPOINT ["python", "train.py"]
