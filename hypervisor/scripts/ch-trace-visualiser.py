#!/bin/env python3
#
# Copyright © 2022 Intel Corporation
#
# SPDX-License-Identifier: Apache-2.0
#

from colorsys import hsv_to_rgb
from random import random
import xml
import json
from sys import argv, stderr
import xml.etree.ElementTree as ET

width = 1000
height = 200
padding = 10

if len(argv) < 3:
    stderr.write("./ch-trace-visualiser <trace file> <output file>\n")
    exit(1)


def nano_time(duration):
    return (duration["secs"] * 10 ** 9) + duration["nanos"]


def duration_to_px_x(start):
    return (nano_time(start) * (width - 2 * padding)) / total_time


def duration_to_px_width(start, end):
    return ((nano_time(end) - nano_time(start)) * (width - 2 * padding)) / total_time


def duration_ms(start, end):
    return (nano_time(end) - nano_time(start)) // 1000000


f = open(argv[1])
report = json.load(f)
total_time = nano_time(report["duration"])

svg = ET.Element("svg", attrib={"width": str(width), "height": str(height),
                                "xmlns": "http://www.w3.org/2000/svg",
                                "xmlns:svg": "http://www.w3.org/2000/svg"
                                })


def add_traced_block(thread_group, depth, traced_block):
    g = ET.SubElement(thread_group, "g",
                      attrib={"transform": "translate(%d,%d)" % (
                          duration_to_px_x(traced_block["timestamp"]),
                          (depth * 18))})
    width = str(duration_to_px_width(
        traced_block["timestamp"], traced_block["end_timestamp"]))

    clip = ET.SubElement(g, "clipPath", attrib={
        "id": "clip_%s" % (traced_block["event"]),
    })
    ET.SubElement(clip, "rect", attrib={
        "width": width,
        "height": "1.5em",
        "x": "0",
        "y": "0"
    })

    (red, green, blue) = hsv_to_rgb(random(), 0.3, 0.75)
    ET.SubElement(g, "rect", attrib={
        "width": width,
        "height": "1.5em",
        "fill": "#%x%x%x" % (int(red * 255), int(green * 255), int(blue * 255))
    })
    text = ET.SubElement(g, "text", attrib={
        "x": "0.2em", "y": "1em", "clip-path": "url(#clip_%s)" % (traced_block["event"])})
    text.text = "%s (%dms)" % (traced_block["event"], duration_ms(
        traced_block["timestamp"], traced_block["end_timestamp"]))


thread_size = (height - (2 * padding)) / len(report["events"])
thread_offset = padding

for thread in report["events"]:
    thread_events = report["events"][thread]
    thread_events = sorted(
        thread_events, key=lambda traced_block: nano_time(traced_block["timestamp"]))
    thread_group = ET.SubElement(
        svg, "g", attrib={"transform": "translate(%d,%d)" % (padding, thread_offset)})
    thread_text = ET.SubElement(thread_group, "text", attrib={
                                "y": "1em"}).text = "Thread: %s" % (thread)
    thread_children = ET.SubElement(thread_group, "g", attrib={
        "transform": "translate(0, 18)"})
    for traced_block in thread_events:
        add_traced_block(thread_children, traced_block["depth"], traced_block)
    thread_offset += thread_size + padding

xml = ET.ElementTree(element=svg)
xml.write(argv[2], xml_declaration=True)
