#ifndef SAILOR_LIGHTS_H
#define SAILOR_LIGHTS_H

// 重定位 macOS 红绿灯（关闭/最小化/全屏）按钮。
// x        ：第一个按钮距窗口左缘的边距
// y        ：按钮组距窗口顶缘的边距
// spacing  ：相邻按钮的间距（中心距 = spacing + 按钮直径）
// 返回 1 表示成功找到并移动了三个按钮，0 表示窗口/按钮尚未就绪。
int sailor_reposition_traffic_lights(double x, double y, double spacing);

#endif
