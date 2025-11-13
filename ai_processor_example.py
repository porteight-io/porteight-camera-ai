#!/usr/bin/env python3
"""
Example AI Frame Processor
This demonstrates how to process frames from camera streams using AI
"""

import sys
from pathlib import Path
from datetime import datetime
import time

# Import the streaming server
from rtmp_to_llhls import set_frame_callback, main

# Example: Using PIL/Pillow for image processing
try:
    from PIL import Image
    import io
    PIL_AVAILABLE = True
except ImportError:
    PIL_AVAILABLE = False
    print("Note: PIL/Pillow not available. Install with: pip install Pillow")

# Example: Using OpenCV for image processing
try:
    import cv2
    import numpy as np
    CV2_AVAILABLE = True
except ImportError:
    CV2_AVAILABLE = False
    print("Note: OpenCV not available. Install with: pip install opencv-python")


# ============================================================================
# YOUR AI PROCESSING FUNCTION
# ============================================================================

def process_frame_with_ai(stream_key, frame_path, frame_data):
    """
    This function is called for EVERY frame (1 per second) from EVERY camera
    
    Args:
        stream_key (str): The camera/stream identifier (e.g., "webcam", "camera1")
        frame_path (Path): Path to the frame image file
        frame_data (bytes): The JPEG image data
    
    Example use cases:
        - Object detection (people, vehicles, animals)
        - Face recognition
        - Activity recognition
        - Anomaly detection
        - License plate recognition
        - Social distancing monitoring
        - PPE detection
        - Fire/smoke detection
    """
    
    timestamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"[{timestamp}] Processing frame from {stream_key}")
    
    # ========================================================================
    # EXAMPLE 1: Simple image info using PIL
    # ========================================================================
    if PIL_AVAILABLE:
        try:
            image = Image.open(io.BytesIO(frame_data))
            width, height = image.size
            print(f"  → Image size: {width}x{height}, Format: {image.format}")
            
            # Example: Save every frame (uncomment if needed)
            # save_path = Path(f"processed_frames/{stream_key}_{int(time.time())}.jpg")
            # save_path.parent.mkdir(exist_ok=True)
            # image.save(save_path)
            
        except Exception as e:
            print(f"  ✗ PIL processing error: {e}")
    
    # ========================================================================
    # EXAMPLE 2: Image analysis using OpenCV
    # ========================================================================
    if CV2_AVAILABLE:
        try:
            # Convert bytes to numpy array
            nparr = np.frombuffer(frame_data, np.uint8)
            img = cv2.imdecode(nparr, cv2.IMREAD_COLOR)
            
            if img is not None:
                height, width, channels = img.shape
                
                # Example: Simple brightness analysis
                gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
                avg_brightness = np.mean(gray)
                print(f"  → OpenCV: {width}x{height}, Brightness: {avg_brightness:.1f}")
                
                # Example: Simple motion detection (uncomment to use)
                # if hasattr(process_frame_with_ai, 'prev_frame'):
                #     diff = cv2.absdiff(gray, process_frame_with_ai.prev_frame)
                #     motion_score = np.mean(diff)
                #     if motion_score > 10:  # Threshold
                #         print(f"  ⚠ Motion detected! Score: {motion_score:.1f}")
                # process_frame_with_ai.prev_frame = gray
                
        except Exception as e:
            print(f"  ✗ OpenCV processing error: {e}")
    
    # ========================================================================
    # EXAMPLE 3: Your AI model integration
    # ========================================================================
    """
    # Example with YOLOv8:
    from ultralytics import YOLO
    model = YOLO('yolov8n.pt')
    results = model(frame_path)
    for r in results:
        boxes = r.boxes
        for box in boxes:
            cls = int(box.cls[0])
            conf = float(box.conf[0])
            print(f"  → Detected: {model.names[cls]} ({conf:.2f})")
    
    # Example with custom TensorFlow/PyTorch model:
    # predictions = your_model.predict(image)
    # process_predictions(predictions)
    
    # Example with cloud AI APIs:
    # results = google_vision_api.detect_objects(frame_data)
    # results = aws_rekognition.detect_labels(frame_data)
    """
    
    # ========================================================================
    # EXAMPLE 4: Store results in database
    # ========================================================================
    """
    # Example: Store detection results
    import sqlite3
    conn = sqlite3.connect('detections.db')
    cursor = conn.cursor()
    cursor.execute('''
        INSERT INTO detections (timestamp, stream_key, object_type, confidence)
        VALUES (?, ?, ?, ?)
    ''', (timestamp, stream_key, 'person', 0.95))
    conn.commit()
    conn.close()
    """
    
    # ========================================================================
    # EXAMPLE 5: Send alerts
    # ========================================================================
    """
    # Example: Send webhook alert
    import requests
    if detected_person:
        requests.post('https://your-webhook.com/alert', json={
            'stream': stream_key,
            'type': 'person_detected',
            'timestamp': timestamp,
            'frame_url': str(frame_path)
        })
    """


# ============================================================================
# ALTERNATIVE: Different processing for different cameras
# ============================================================================

def multi_camera_ai_processor(stream_key, frame_path, frame_data):
    """
    Example: Different AI processing based on camera location
    """
    
    if stream_key == "entrance_camera":
        # People counting, face recognition
        process_entrance_camera(frame_path, frame_data)
    
    elif stream_key == "parking_camera":
        # License plate recognition, parking spot detection
        process_parking_camera(frame_path, frame_data)
    
    elif stream_key == "warehouse_camera":
        # PPE detection, forklift detection
        process_warehouse_camera(frame_path, frame_data)
    
    else:
        # Default processing
        process_frame_with_ai(stream_key, frame_path, frame_data)


def process_entrance_camera(frame_path, frame_data):
    print(f"Processing entrance camera: {frame_path}")
    # Your entrance-specific AI logic here
    pass


def process_parking_camera(frame_path, frame_data):
    print(f"Processing parking camera: {frame_path}")
    # Your parking-specific AI logic here
    pass


def process_warehouse_camera(frame_path, frame_data):
    print(f"Processing warehouse camera: {frame_path}")
    # Your warehouse-specific AI logic here
    pass


# ============================================================================
# PERFORMANCE TIPS
# ============================================================================
"""
1. Keep processing fast: Each frame should process in < 1 second
   - Use GPU acceleration when possible
   - Use lightweight models for real-time processing
   - Consider running heavy processing asynchronously

2. Handle errors gracefully: Don't crash on bad frames
   - Wrap AI processing in try-except blocks
   - Log errors but continue processing

3. Use threading for heavy operations:
   from concurrent.futures import ThreadPoolExecutor
   executor = ThreadPoolExecutor(max_workers=4)
   
   def async_process(stream_key, frame_path, frame_data):
       executor.submit(heavy_ai_processing, stream_key, frame_path, frame_data)

4. Batch processing: If you have multiple cameras
   - Process frames in batches when possible
   - Use GPU batch inference

5. Skip frames if needed:
   frame_counter = 0
   def smart_processor(stream_key, frame_path, frame_data):
       global frame_counter
       frame_counter += 1
       if frame_counter % 5 == 0:  # Process every 5th frame only
           process_frame_with_ai(stream_key, frame_path, frame_data)
"""


# ============================================================================
# MAIN
# ============================================================================

if __name__ == '__main__':
    print("=" * 60)
    print("AI Frame Processor Example")
    print("=" * 60)
    print()
    print("This will process 1 frame per second from each camera stream")
    print("Edit the process_frame_with_ai() function to add your AI logic")
    print()
    
    # Register your AI processing function
    # Choose one of these:
    
    # Option 1: Simple single processor
    set_frame_callback(process_frame_with_ai)
    
    # Option 2: Multi-camera processor with different logic per camera
    # set_frame_callback(multi_camera_ai_processor)
    
    print("✓ AI frame processor registered")
    print()
    
    # Start the streaming server
    # This will also start frame extraction and call your callback
    main()

