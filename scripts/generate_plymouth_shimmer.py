#!/usr/bin/env python3
"""
Generate smooth specular shimmer frames for the XeroxGo Plymouth boot splash.
Applies a traveling specular light beam with Gaussian falloff strictly masked
to the logo alpha channel, creating an Apple / Android style text shimmer.
"""

import os
import sys
import numpy as np
from PIL import Image

def generate_shimmer_frames(
    input_logo_path: str,
    output_dir: str,
    total_frames: int = 36,
    target_width: int = 600,
    slant_deg: float = 20.0,
    core_sigma: float = 32.0,
    ambient_sigma: float = 75.0,
    boost_val: float = 190.0,
):
    os.makedirs(output_dir, exist_ok=True)
    raw_img = Image.open(input_logo_path).convert("RGBA")
    
    # Scale proportionally to target_width
    orig_w, orig_h = raw_img.size
    aspect = orig_h / orig_w
    target_h = int(round(target_width * aspect))
    
    base_img = raw_img.resize((target_width, target_h), Image.Resampling.LANCZOS)
    arr = np.array(base_img, dtype=np.float32)
    H, W, _ = arr.shape
    
    # Slanted beam projection coordinate: p = x + y * tan(slant)
    slant = np.tan(np.radians(slant_deg))
    Y, X = np.ogrid[:H, :W]
    P = X + Y * slant
    
    c_start = -100.0
    c_end = W + H * slant + 100.0
    
    print(f"Generating {total_frames} shimmer frames at {W}x{H}...")
    
    for i in range(total_frames):
        t = i / (total_frames - 1)
        c = c_start + t * (c_end - c_start)
        dist = np.abs(P - c)
        
        # Dual-Gaussian intensity: sharp specular core + soft atmospheric sheen
        intensity = 0.75 * np.exp(-0.5 * (dist / core_sigma)**2) + 0.25 * np.exp(-0.5 * (dist / ambient_sigma)**2)
        
        # In first and last frames, force intensity to 0 for seamless loop
        if i == 0 or i == total_frames - 1:
            intensity = np.zeros_like(intensity)
            
        frame = np.copy(arr)
        # Specular boost to RGB channels
        for ch in range(3):
            boost = intensity * boost_val
            frame[:, :, ch] = np.clip(arr[:, :, ch] + boost, 0.0, 255.0)
            
        # Strictly preserve original alpha channel (zero background spill)
        frame[:, :, 3] = arr[:, :, 3]
        
        out_img = Image.fromarray(frame.astype(np.uint8))
        out_path = os.path.join(output_dir, f"shimmer-{i}.png")
        out_img.save(out_path, optimize=True)
        
    # Also save standard logo.png identical to frame 0
    base_img.save(os.path.join(output_dir, "logo.png"), optimize=True)
    print(f"All {total_frames} shimmer frames successfully created in {output_dir}")

if __name__ == "__main__":
    script_dir = os.path.dirname(os.path.abspath(__file__))
    repo_root = os.path.abspath(os.path.join(script_dir, ".."))
    
    src_logo = os.path.join(repo_root, "frontend/assets/xeroxgo-logo.png")
    dest_dir = os.path.join(repo_root, "kiosk-daemon/plymouth/xeroxgo")
    
    generate_shimmer_frames(src_logo, dest_dir, total_frames=36)

