#import <Cocoa/Cocoa.h>
#import "lights.h"

// Wails v2 没有暴露红绿灯位置配置；这里直接取 NSWindow 的三个
// standardWindowButton 重设 frame。AppKit 强制要求窗口几何只能在主线程
// 修改（后台调用会抛 NSInternalInconsistencyException），因此整个操作
// dispatch_sync 到主队列执行。
int sailor_reposition_traffic_lights(double x, double y, double spacing) {
    __block int result = 0;
    dispatch_sync(dispatch_get_main_queue(), ^{
        NSWindow *win = [NSApp mainWindow];
        if (win == nil) {
            for (NSWindow *w in [NSApp windows]) {
                if ([w isVisible]) { win = w; break; }
            }
        }
        if (win == nil) return;

        NSButton *close = [win standardWindowButton:NSWindowCloseButton];
        NSButton *mini  = [win standardWindowButton:NSWindowMiniaturizeButton];
        NSButton *zoom  = [win standardWindowButton:NSWindowZoomButton];
        if (close == nil || mini == nil || zoom == nil) return;

        NSView *container = close.superview;
        if (container == nil) return;

        CGFloat h = container.frame.size.height;
        BOOL flipped = container.isFlipped;
        CGFloat cy = flipped ? y : (h - y - close.frame.size.height);

        CGRect f = close.frame;
        f.origin.x = x;
        f.origin.y = cy;
        close.frame = f;

        f = mini.frame;
        f.origin.x = x + spacing;
        f.origin.y = cy;
        mini.frame = f;

        f = zoom.frame;
        f.origin.x = x + spacing * 2;
        f.origin.y = cy;
        zoom.frame = f;
        result = 1;
    });
    return result;
}
