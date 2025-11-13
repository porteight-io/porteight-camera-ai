#!/usr/bin/env python3
"""
YOLO Object Detection Example
Real-time object detection on camera streams using YOLOv8
"""

from pathlib import Path
from datetime import datetime
import json

# Import the streaming server
from rtmp_to_llhls import set_frame_callback, main

try:
    from ultralytics import YOLO
    import cv2
    import numpy as np
    YOLO_AVAILABLE = True
except ImportError:
    YOLO_AVAILABLE = False
    print("ERROR: YOLOv8 not available!")
    print("Install with: pip install ultralytics opencv-python")
    exit(1)

# Configuration
YOLO_MODEL = "yolov8n.pt"  # Options: yolov8n, yolov8s, yolov8m, yolov8l, yolov8x
CONFIDENCE_THRESHOLD = 0.5
SAVE_DETECTIONS = True
DETECTIONS_DIR = Path("detections")

# Load YOLO model once at startup
print(f"Loading YOLO model: {YOLO_MODEL}...")
model = YOLO(YOLO_MODEL)
print("✓ YOLO model loaded successfully")

# Create detections directory
if SAVE_DETECTIONS:
    DETECTIONS_DIR.mkdir(exist_ok=True)
    (DETECTIONS_DIR / "logs").mkdir(exist_ok=True)
    (DETECTIONS_DIR / "images").mkdir(exist_ok=True)


def yolo_frame_processor(stream_key, frame_path, frame_data):
    """
    Process each frame with YOLO object detection
    """
    try:
        timestamp = datetime.now()
        timestamp_str = timestamp.strftime("%Y-%m-%d %H:%M:%S")
        
        # Decode image
        nparr = np.frombuffer(frame_data, np.uint8)
        img = cv2.imdecode(nparr, cv2.IMREAD_COLOR)
        
        if img is None:
            print(f"[{timestamp_str}] {stream_key}: Failed to decode frame")
            return
        
        # Run YOLO inference
        results = model(img, conf=CONFIDENCE_THRESHOLD, verbose=False)
        
        # Process results
        detections = []
        for r in results:
            boxes = r.boxes
            for box in boxes:
                cls = int(box.cls[0])
                conf = float(box.conf[0])
                xyxy = box.xyxy[0].cpu().numpy()
                
                detection = {
                    'class': model.names[cls],
                    'confidence': round(conf, 2),
                    'bbox': [int(x) for x in xyxy]
                }
                detections.append(detection)
        
        # Log detections
        if detections:
            print(f"[{timestamp_str}] {stream_key}: {len(detections)} objects detected")
            for det in detections:
                print(f"  → {det['class']}: {det['confidence']}")
            
            # Save detection log
            if SAVE_DETECTIONS:
                log_entry = {
                    'timestamp': timestamp.isoformat(),
                    'stream_key': stream_key,
                    'detections': detections
                }
                
                log_file = DETECTIONS_DIR / "logs" / f"{stream_key}_{timestamp.strftime('%Y%m%d')}.jsonl"
                with open(log_file, 'a') as f:
                    f.write(json.dumps(log_entry) + '\n')
                
                # Optionally save annotated image
                # annotated_frame = results[0].plot()
                # img_file = DETECTIONS_DIR / "images" / f"{stream_key}_{int(timestamp.timestamp())}.jpg"
                # cv2.imwrite(str(img_file), annotated_frame)
        else:
            print(f"[{timestamp_str}] {stream_key}: No objects detected")
            
    except Exception as e:
        print(f"Error processing frame from {stream_key}: {e}")


if __name__ == '__main__':
    print("=" * 60)
    print("YOLO Object Detection on Live Camera Streams")
    print("=" * 60)
    print(f"Model: {YOLO_MODEL}")
    print(f"Confidence threshold: {CONFIDENCE_THRESHOLD}")
    print(f"Save detections: {SAVE_DETECTIONS}")
    if SAVE_DETECTIONS:
        print(f"Detections directory: {DETECTIONS_DIR}")
    print("=" * 60)
    print()
    
    # Register YOLO processor
    set_frame_callback(yolo_frame_processor)
    
    print("✓ YOLO frame processor registered")
    print("Starting streaming server...")
    print()
    
    # Start the streaming server
    main()

