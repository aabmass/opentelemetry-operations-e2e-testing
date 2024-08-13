from flask import Flask, request

app = Flask(__name__)


@app.route("/", methods=["GET", "POST"])
def hello_world():
    response = f"""
    <p1>Hello world! Got request:<p1>

    <p>
    <code>
    {request.url}
    </code>
    </p>

    <p>
    <code>
    {request.get_data()}
    </code>
    </p>

    <p>
    <code>
    {request.headers}
    </code>
    </p>
    """
    print(response)
    return response


if __name__ == "__main__":
    app.run(debug=True, host="0.0.0.0", port=8080)
