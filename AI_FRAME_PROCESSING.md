# AI Frame Processing Guide

## Overview

The streaming server now automatically extracts **1 frame per second** from each camera stream and makes it available for AI processing. This allows you to run real-time AI analysis (object detection, face recognition, etc.) on your camera feeds without affecting the live streaming or recording functionality.

## Architecture

```
Camera Stream (RTMP) → Server
                       ├─→ HLS (Live streaming for viewers)
                       ├─→ Recording (5-min MP4 segments)
                       └─→ Frame Extraction (1 fps for AI)
                               ↓
                          Your AI Callback
                          (Process each frame)
```

## How It Works

1. **Frame Extraction**: FFmpeg extracts 1 frame per second from each stream
2. **File Output**: Latest frame is saved as `frames/{stream_key}/latest.jpg`
3. **Callback Trigger**: Your callback function is called for each new frame
4. **AI Processing**: Your code analyzes the frame (object detection, etc.)

## Quick Start

### Basic Example

```python
from rtmp_to_llhls import set_frame_callback, main

def my_ai_processor(stream_key, frame_path, frame_data):
    """
    This function is called once per second for each camera
    
    Args:
        stream_key: Camera identifier (e.g., "webcam")
        frame_path: Path to frame file (Path object)
        frame_data: JPEG image data (bytes)
    """
    print(f"Processing frame from {stream_key}")
    # Your AI processing here

# Register your callback
set_frame_callback(my_ai_processor)

# Start server (handles streaming + AI processing)
main()
```

### Run Your AI Processor

```bash
# Run with your custom AI processing
python3 your_ai_script.py

# Or use the provided examples
python3 ai_processor_example.py
python3 ai_yolo_example.py
```

## Examples Provided

### 1. `ai_processor_example.py`
Comprehensive example showing:
- PIL/Pillow image processing
- OpenCV integration
- Multiple camera handling
- Database logging
- Alert systems

### 2. `ai_yolo_example.py`
Ready-to-use YOLO object detection:
- Real-time object detection on all streams
- JSON logging of detections
- Configurable confidence threshold
- Performance optimized

## Common Use Cases

### Object Detection (YOLO)

```python
from ultralytics import YOLO
from rtmp_to_llhls import set_frame_callback, main

model = YOLO('yolov8n.pt')

def detect_objects(stream_key, frame_path, frame_data):
    results = model(frame_path, conf=0.5)
    
    for r in results:
        for box in r.boxes:
            cls = int(box.cls[0])
            conf = float(box.conf[0])
            print(f"{stream_key}: Detected {model.names[cls]} ({conf:.2f})")

set_frame_callback(detect_objects)
main()
```

### Face Recognition

```python
import face_recognition
import numpy as np
from PIL import Image
import io

# Load known faces
known_face_encodings = [...]
known_face_names = [...]

def recognize_faces(stream_key, frame_path, frame_data):
    # Load image
    image = face_recognition.load_image_file(io.BytesIO(frame_data))
    
    # Find faces
    face_locations = face_recognition.face_locations(image)
    face_encodings = face_recognition.face_encodings(image, face_locations)
    
    for face_encoding in face_encodings:
        matches = face_recognition.compare_faces(known_face_encodings, face_encoding)
        name = "Unknown"
        
        if True in matches:
            first_match_index = matches.index(True)
            name = known_face_names[first_match_index]
        
        print(f"{stream_key}: Detected {name}")

set_frame_callback(recognize_faces)
main()
```

### License Plate Recognition

```python
import easyocr

reader = easyocr.Reader(['en'])

def detect_license_plates(stream_key, frame_path, frame_data):
    results = reader.readtext(str(frame_path))
    
    for (bbox, text, prob) in results:
        # Filter for license plate patterns
        if is_license_plate_format(text) and prob > 0.5:
            print(f"{stream_key}: License plate detected: {text}")
            # Send alert, log to database, etc.

set_frame_callback(detect_license_plates)
main()
```

### Motion Detection

```python
import cv2
import numpy as np

# Store previous frames
previous_frames = {}

def detect_motion(stream_key, frame_path, frame_data):
    # Decode frame
    nparr = np.frombuffer(frame_data, np.uint8)
    frame = cv2.imdecode(nparr, cv2.IMREAD_GRAYSCALE)
    
    if stream_key not in previous_frames:
        previous_frames[stream_key] = frame
        return
    
    # Calculate difference
    diff = cv2.absdiff(frame, previous_frames[stream_key])
    motion_score = np.mean(diff)
    
    if motion_score > 15:  # Threshold
        print(f"{stream_key}: Motion detected! Score: {motion_score:.1f}")
    
    previous_frames[stream_key] = frame

set_frame_callback(detect_motion)
main()
```

## Advanced Features

### Multi-Camera with Different Processing

```python
def process_by_camera(stream_key, frame_path, frame_data):
    if stream_key == "entrance":
        # People counting
        count_people(frame_path, frame_data)
    
    elif stream_key == "parking":
        # License plate recognition
        detect_plates(frame_path, frame_data)
    
    elif stream_key == "warehouse":
        # PPE detection
        check_safety_equipment(frame_path, frame_data)

set_frame_callback(process_by_camera)
main()
```

### Async Processing for Heavy AI Models

```python
from concurrent.futures import ThreadPoolExecutor
import time

executor = ThreadPoolExecutor(max_workers=4)

def heavy_ai_processing(stream_key, frame_path, frame_data):
    # Your heavy AI model
    time.sleep(2)  # Simulating slow processing
    print(f"Finished processing {stream_key}")

def async_processor(stream_key, frame_path, frame_data):
    # Submit to thread pool - don't block
    executor.submit(heavy_ai_processing, stream_key, frame_path, frame_data)

set_frame_callback(async_processor)
main()
```

### Database Logging

```python
import sqlite3
from datetime import datetime

# Setup database
conn = sqlite3.connect('detections.db')
conn.execute('''
    CREATE TABLE IF NOT EXISTS detections (
        id INTEGER PRIMARY KEY,
        timestamp TEXT,
        stream_key TEXT,
        object_type TEXT,
        confidence REAL
    )
''')
conn.close()

def log_detections(stream_key, frame_path, frame_data):
    # Your AI detection
    detections = run_detection(frame_data)
    
    # Log to database
    conn = sqlite3.connect('detections.db')
    for det in detections:
        conn.execute(
            'INSERT INTO detections VALUES (NULL, ?, ?, ?, ?)',
            (datetime.now().isoformat(), stream_key, det['class'], det['conf'])
        )
    conn.commit()
    conn.close()

set_frame_callback(log_detections)
main()
```

### Send Alerts

```python
import requests

WEBHOOK_URL = "https://your-webhook.com/alert"

def alert_on_detection(stream_key, frame_path, frame_data):
    # Your detection logic
    if person_detected:
        requests.post(WEBHOOK_URL, json={
            'stream': stream_key,
            'type': 'person_detected',
            'timestamp': datetime.now().isoformat(),
            'confidence': 0.95
        })

set_frame_callback(alert_on_detection)
main()
```

## Performance Optimization

### 1. Keep Processing Fast
Each frame should process in < 1 second to keep up with the 1 fps rate.

```python
import time

def optimized_processor(stream_key, frame_path, frame_data):
    start_time = time.time()
    
    # Your AI processing
    results = model(frame_path)
    
    processing_time = time.time() - start_time
    if processing_time > 1.0:
        print(f"Warning: Processing took {processing_time:.2f}s")
```

### 2. Use GPU Acceleration
```python
import torch

# Check if CUDA is available
device = 'cuda' if torch.cuda.is_available() else 'cpu'
model = YOLO('yolov8n.pt').to(device)
```

### 3. Use Lightweight Models
- YOLOv8n (nano) instead of YOLOv8x
- MobileNet instead of ResNet
- Quantized models for faster inference

### 4. Skip Frames if Needed
```python
frame_counter = 0

def smart_processor(stream_key, frame_path, frame_data):
    global frame_counter
    frame_counter += 1
    
    # Process every 5th frame (0.2 fps instead of 1 fps)
    if frame_counter % 5 == 0:
        heavy_ai_processing(stream_key, frame_path, frame_data)
```

### 5. Batch Processing for Multiple Cameras
```python
import queue
import threading

frame_queue = queue.Queue(maxsize=10)

def collector(stream_key, frame_path, frame_data):
    frame_queue.put((stream_key, frame_data))

def batch_processor():
    batch = []
    while True:
        try:
            item = frame_queue.get(timeout=1)
            batch.append(item)
            
            if len(batch) >= 4:  # Process 4 frames at once
                # Batch inference
                process_batch(batch)
                batch = []
        except queue.Empty:
            if batch:
                process_batch(batch)
                batch = []

# Start batch processor thread
threading.Thread(target=batch_processor, daemon=True).start()
set_frame_callback(collector)
main()
```

## Frame Directory Structure

```
frames/
├── webcam/
│   └── latest.jpg          # Always the latest frame (overwritten)
├── camera1/
│   └── latest.jpg
└── camera2/
    └── latest.jpg
```

## Configuration

You can modify these settings in `rtmp_to_llhls.py`:

```python
FRAME_EXTRACTION_FPS = 1  # Change to 2 for 2 fps, 0.5 for 1 frame every 2 seconds
FRAMES_DIR = Path("frames")  # Change output directory
```

## Error Handling

Always wrap your AI processing in try-except:

```python
def safe_processor(stream_key, frame_path, frame_data):
    try:
        # Your AI processing
        results = model(frame_path)
        process_results(results)
    except Exception as e:
        print(f"Error processing {stream_key}: {e}")
        # Log error but continue processing next frames
```

## Installation Requirements

### Basic
```bash
pip install Pillow opencv-python
```

### For YOLO
```bash
pip install ultralytics opencv-python
```

### For Face Recognition
```bash
pip install face-recognition
```

### For OCR/License Plates
```bash
pip install easyocr
```

### For TensorFlow
```bash
pip install tensorflow
```

### For PyTorch
```bash
pip install torch torchvision
```

## FAQ

**Q: Does frame extraction affect live streaming?**
A: No, frame extraction runs in a separate FFmpeg process and doesn't impact HLS or recording.

**Q: Can I change the extraction rate?**
A: Yes, modify `FRAME_EXTRACTION_FPS` in `rtmp_to_llhls.py`.

**Q: What happens if my AI processing is slower than 1 second?**
A: Frames will queue up. Use async processing or skip frames to avoid delays.

**Q: Can I save the frames permanently?**
A: Yes, in your callback save the frame to a different location:
```python
def save_frames(stream_key, frame_path, frame_data):
    save_path = Path(f"saved_frames/{stream_key}_{int(time.time())}.jpg")
    save_path.parent.mkdir(exist_ok=True)
    save_path.write_bytes(frame_data)
```

**Q: Can I use cloud AI APIs?**
A: Yes! Just call the API in your callback:
```python
def cloud_ai(stream_key, frame_path, frame_data):
    response = requests.post('https://api.example.com/detect', 
                            files={'image': frame_data})
    results = response.json()
```

## Next Steps

1. Choose an example or create your own processor
2. Test with a single camera first
3. Optimize for your hardware
4. Scale to multiple cameras
5. Add database logging and alerts

Happy AI processing! 🚀

