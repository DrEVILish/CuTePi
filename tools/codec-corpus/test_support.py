"""Unit tests for support.py's GPU-wall measurement helpers (stdlib only).

    python3 -m unittest discover -s tools/codec-corpus -p 'test_*.py'
"""
import os
import sys
import unittest

sys.argv = sys.argv[:1]  # support.py reads its options at import
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import support  # noqa: E402

# A sample is (time, shown_frames, shown_steps, presented, route, caps).


def smp(t, frames, steps, presented):
    return (t, frames, steps, presented, "isp", "")


class GLPerRefresh(unittest.TestCase):
    def test_counts_per_presented_frame_scaled_to_rate(self):
        # 30 new frames over 60 presented, at 60 Hz: 30 a second, whatever the
        # polling jitter in the timestamps.
        s = [smp(0.0, 0, 0, 0), smp(0.37, 10, 20, 20), smp(1.03, 30, 60, 60)]
        self.assertAlmostEqual(support.gl_per_refresh(s, 0, 2, 1, 60.0), 30.0)
        self.assertAlmostEqual(support.gl_per_refresh(s, 0, 2, 2, 60.0), 60.0)

    def test_window_bounds_pick_inside_samples_only(self):
        s = [smp(0.0, 0, 0, 0), smp(0.5, 30, 30, 30), smp(1.0, 30, 60, 60), smp(1.5, 60, 90, 90)]
        # [0.4, 1.1): samples at 0.5 and 1.0 only: no new frames, 30 steps over 30.
        self.assertEqual(support.gl_per_refresh(s, 0.4, 1.1, 1, 60.0), 0.0)
        self.assertAlmostEqual(support.gl_per_refresh(s, 0.4, 1.1, 2, 60.0), 60.0)

    def test_fewer_than_two_samples_is_unknown(self):
        self.assertIsNone(support.gl_per_refresh([], 0, 1, 1, 60.0))
        self.assertIsNone(support.gl_per_refresh([smp(0.5, 1, 1, 1)], 0, 1, 1, 60.0))

    def test_presented_not_advancing_is_unknown_not_a_division_error(self):
        s = [smp(0.0, 0, 0, 100), smp(1.0, 0, 0, 100)]
        self.assertIsNone(support.gl_per_refresh(s, 0, 2, 1, 60.0))


class GLRate(unittest.TestCase):
    def test_rate_per_second(self):
        s = [smp(1.0, 0, 0, 0), smp(3.0, 0, 0, 120)]
        self.assertAlmostEqual(support.gl_rate(s, 0, 4, 3), 60.0)

    def test_same_timestamp_is_unknown(self):
        self.assertIsNone(support.gl_rate([smp(1.0, 0, 0, 0), smp(1.0, 0, 0, 5)], 0, 2, 3))

    def test_empty_window_is_unknown(self):
        self.assertIsNone(support.gl_rate([smp(1.0, 0, 0, 0), smp(2.0, 0, 0, 5)], 5, 6, 3))


if __name__ == "__main__":
    unittest.main()
