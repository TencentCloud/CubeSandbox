#include <iostream>
#include <string>

#include "greeting.h"

int main(int argc, char* argv[]) {
    const std::string name = argc > 1 ? argv[1] : "world";
    std::cout << greeting_for(name) << '\n';
    return 0;
}
